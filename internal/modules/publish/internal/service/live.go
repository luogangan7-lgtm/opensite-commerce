// Responsibility: attach live purchasable SKUs to the sealed release product views.
// Dependencies: publish contract, internal/pricing (approved port), stdlib; caller: publish facade.
// Invariants: product content stays sealed; price/availability are live; availability
// is a boolean only; a SKU without an applicable anonymous price is not purchasable.
package service

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

// MaxSKUsPerProduct bounds one product's purchasable SKUs; more fails closed like other bounds.
const MaxSKUsPerProduct = 100

// LiveSKU is one active SKU of an active product listed in the current release.
type LiveSKU struct {
	ProductID, SKUID, Code string
	Options                []contract.OptionValue
	Available              bool
}

// Live is read in the same repeatable-read transaction as the sealed release.
type Live struct {
	TenantID   string
	At         time.Time
	SKUs       []LiveSKU
	PriceLists []pricing.PriceList
}

// AttachSKUs prices each SKU for quantity 1 in the product's release currency using
// only active audience=all lists (the anonymous/default price rule) via pricing.Quote.
func AttachSKUs(site contract.Site, live Live) (contract.Site, error) {
	fail := func() (contract.Site, error) { return contract.Site{}, contract.ErrUnavailable }
	index := map[string]int{}
	products := make([]contract.Product, len(site.Products))
	for i, p := range site.Products {
		p.SKUs = []contract.SKU{}
		products[i] = p
		index[strings.ToLower(p.ID)] = i
	}
	for _, s := range live.SKUs {
		i, ok := index[strings.ToLower(s.ProductID)]
		if !ok {
			continue // the SQL scope already limits SKUs to release products
		}
		if !text(s.SKUID, 128, true) || !text(s.Code, 255, true) || s.Options == nil || len(s.Options) > 20 {
			return fail()
		}
		for _, o := range s.Options {
			if !text(o.Name, 255, true) || !text(o.Value, 255, false) {
				return fail()
			}
		}
		p := &products[i]
		quote, err := pricing.Quote(live.PriceLists, pricing.Request{TenantID: live.TenantID, SKUID: s.SKUID, Currency: p.Currency, Quantity: 1, At: live.At})
		if errors.Is(err, pricing.ErrNoPrice) {
			continue
		}
		if err != nil || quote.Unit.Amount < 0 || len(p.SKUs) == MaxSKUsPerProduct {
			return fail()
		}
		p.SKUs = append(p.SKUs, contract.SKU{SKUID: s.SKUID, SKUCode: s.Code, OptionValues: s.Options, PriceMinor: strconv.FormatInt(quote.Unit.Amount, 10), Currency: quote.Unit.Currency, Available: s.Available})
	}
	site.Products = products
	return withinBudget(site)
}
