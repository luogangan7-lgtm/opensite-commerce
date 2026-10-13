// Responsibility: validate a trusted cart and recompute its price/tax/shipping.
// Dependencies: public calculators and checkout contract; called before Orders.
// Invariants: ignores all cart price projections, checked integer totals, no IO.
package service

import (
	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type quote struct {
	Lines    []orders.Line
	Net, Tax int64
	Shipping shipping.Result
}

func calculate(cart contract.Cart, rules contract.Rules, address contract.ShippingAddress) (quote, error) {
	var q quote
	if !tenanttx.ValidTenantID(rules.TenantID) || !tenanttx.ValidTenantID(cart.ID) || cart.Version <= 0 || len(cart.Lines) == 0 || len(cart.Lines) > 1000 || rules.At.IsZero() {
		return q, contract.ErrInvalid
	}
	ids := map[string]bool{}
	skus := map[string]bool{}
	weight := money.Money{Currency: rules.Currency}
	subtotal := money.Money{Currency: rules.Currency}
	items := make([]tax.LineItem, 0, len(cart.Lines))
	for _, l := range cart.Lines {
		if !tenanttx.ValidTenantID(l.ID) || !tenanttx.ValidTenantID(l.SKUID) || l.Quantity <= 0 || ids[l.ID] || skus[l.SKUID] {
			return quote{}, contract.ErrInvalid
		}
		ids[l.ID] = true
		skus[l.SKUID] = true
		metadata, ok := rules.SKUs[l.SKUID]
		if !ok || metadata.ID != l.SKUID || metadata.WeightG < 0 || metadata.Code == "" || metadata.TaxCategory == "" {
			return quote{}, contract.ErrInvalid
		}
		price, e := pricing.Quote(rules.PriceLists, pricing.Request{TenantID: rules.TenantID, SKUID: l.SKUID, Currency: rules.Currency, Quantity: l.Quantity, At: rules.At, CustomerGroupID: rules.CustomerGroupID})
		if e != nil {
			return quote{}, e
		}
		subtotal, e = subtotal.Add(price.Line)
		if e != nil {
			return quote{}, e
		}
		w, e := (money.Money{Amount: metadata.WeightG, Currency: rules.Currency}).Mul(l.Quantity)
		if e != nil {
			return quote{}, e
		}
		weight, e = weight.Add(w)
		if e != nil {
			return quote{}, e
		}
		q.Lines = append(q.Lines, orders.Line{ID: l.ID, SKUID: l.SKUID, SKU: metadata.Code, Quantity: l.Quantity, Price: price})
		items = append(items, tax.LineItem{Currency: rules.Currency, ID: l.ID, Category: metadata.TaxCategory, UnitAmount: price.Unit.Amount, Quantity: l.Quantity})
	}
	taxed, e := tax.Calculate(tax.Destination{Country: address.Country, Subdivision: address.Subdivision}, items, rules.PricesIncludeTax, rules.At, rules.Tax)
	if e != nil {
		return quote{}, e
	}
	for i := range q.Lines {
		q.Lines[i].Tax = taxed.Lines[i]
	}
	q.Net, q.Tax = taxed.Totals.Net, taxed.Totals.Tax
	// Free-shipping thresholds use the exact priced cart subtotal, before fees.
	q.Shipping, e = shipping.Quote(rules.ShippingZones, rules.ShippingRates, shipping.Request{ProfileID: rules.ShippingProfileID, Destination: shipping.Destination{Country: address.Country, Subdivision: address.Subdivision}, WeightG: weight.Amount, Subtotal: subtotal})
	if e != nil {
		return quote{}, e
	}
	total, e := (money.Money{Amount: taxed.Totals.Gross, Currency: rules.Currency}).Add(q.Shipping.Charge)
	if e != nil {
		return quote{}, e
	}
	if total.Amount <= 0 {
		return quote{}, contract.ErrInvalid
	}
	return q, nil
}

// Stable order/line UUIDs are derived only after admission, never from request IDs.
func orderID(sessionID string) string {
	return uuid.NewSHA1(uuid.MustParse(sessionID), []byte("checkout-order:v1")).String()
}
