// Responsibility: adopt an approved negotiated snapshot into one immutable order.
// Dependencies: quotes transactions, public Orders creator and injected checkout port.
// Callers: authorized quote-order HTTP. Invariants: DB-clock validity and exact
// receipt binding; provider IO belongs to checkout and never runs inside this tx.
package quotes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

type ConversionConfig struct {
	Runner   TenantRunner
	Quotes   c.ConversionRepository
	Orders   oc.QuoteCreator
	Checkout c.PrepaymentPort
}
type ConversionApplication struct{ config ConversionConfig }

func NewConversionApplication(config ConversionConfig) *ConversionApplication {
	if config.Quotes == nil {
		config.Quotes = New()
	}
	return &ConversionApplication{config: config}
}

func conversionKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, x := range key {
		if x < '!' || x > '~' {
			return false
		}
	}
	return true
}

// Orders creation keys span the tenant; quote receipts retain the buyer's raw
// key. A JSON tuple preserves field boundaries and the digest fits Orders' cap.
func conversionOrderKey(tenant, actorKind, actorID, quoteID, rawKey string) string {
	body, _ := json.Marshal([]string{"quotes.orders.v1", tenant, actorKind, actorID, quoteID, rawKey})
	return fmt.Sprintf("quotes.orders.v1:%x", sha256.Sum256(body))
}

func (a *ConversionApplication) Convert(ctx context.Context, quoteID, key string) (out c.Conversion, err error) {
	actor, err := c.ActorFromContext(ctx)
	if err != nil {
		return out, err
	}
	if actor.Kind() != "customer" {
		return out, c.ErrForbidden
	}
	if !c.ValidID(quoteID) || !conversionKey(key) {
		return out, c.ErrInvalid
	}
	if a.config.Runner == nil || a.config.Orders == nil || a.config.Checkout == nil {
		return out, c.ErrUnavailable
	}
	body, _ := json.Marshal([]string{"quotes.orders.v1", actor.TenantID(), actor.CompanyID(), actor.ID(), quoteID})
	hash := sha256.Sum256(body)
	replay := false
	err = a.config.Runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		q, e := a.config.Quotes.LockForConversion(ctx, tx, actor, quoteID)
		if e != nil {
			return e
		}
		existing, e := a.config.Quotes.GetConversion(ctx, tx, actor, quoteID)
		if e == nil {
			if existing.IdempotencyKey != key || existing.CustomerID != actor.ID() {
				return c.ErrConflict
			}
			out = existing
			replay = true
			return nil
		}
		if !errors.Is(e, c.ErrNotFound) {
			return e
		}
		labels := make(map[string]string, len(q.Snapshot.Lines))
		for _, line := range q.Snapshot.Lines {
			if !c.ValidID(line.VariantID) || !c.ValidID(line.ProductID) {
				return c.ErrInvalid
			}
			label, e := a.config.Quotes.ConversionSKU(ctx, tx, actor, line.ProductID, line.VariantID)
			if e != nil {
				return e
			}
			labels[line.VariantID] = label
		}
		orderKey := conversionOrderKey(actor.TenantID(), actor.Kind(), actor.ID(), quoteID, key)
		in, e := quoteOrder(q, uuid.NewString(), actor.ID(), orderKey, labels)
		if e != nil {
			return e
		}
		order, e := a.config.Orders.CreateFromQuote(ctx, tx, actor.TenantID(), in)
		if e != nil {
			return conversionOrderError(e)
		}
		out, e = a.config.Quotes.BindConversion(ctx, tx, actor, quoteID, order.ID, q.Version, key, hash[:])
		return e
	})
	if err != nil {
		return c.Conversion{}, err
	}
	// The durable order projection is the replay result once payment has left
	// pending. Fulfillment and refunds must never restart the payment port.
	if replay && out.State != string(oc.PendingPayment) {
		return out, nil
	}
	paid, err := a.config.Checkout.Prepay(ctx, actor, out)
	if err != nil {
		return out, err
	}
	if paid.OrderID != out.OrderID || paid.Amount != out.Amount || (paid.State != string(oc.PendingPayment) && paid.State != string(oc.Paid) && paid.State != string(oc.Cancelled)) || (paid.State != string(oc.Cancelled) && paid.ProviderRef == "") {
		return out, c.ErrUnavailable
	}
	// A provider result is never payment proof: state is re-read from Orders via
	// the actor-filtered conversion projection after checkout has committed.
	current, err := a.GetConversion(ctx, quoteID)
	if err != nil {
		return out, err
	}
	if current.OrderID != out.OrderID || current.Amount != out.Amount || current.State != paid.State {
		return out, c.ErrUnavailable
	}
	current.ProviderRef = paid.ProviderRef
	return current, nil
}

func (a *ConversionApplication) GetConversion(ctx context.Context, quoteID string) (out c.Conversion, err error) {
	actor, err := c.ActorFromContext(ctx)
	if err != nil {
		return out, err
	}
	if !c.ValidID(quoteID) {
		return out, c.ErrInvalid
	}
	if a.config.Runner == nil {
		return out, c.ErrUnavailable
	}
	err = a.config.Runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		out, err = a.config.Quotes.GetConversion(ctx, tx, actor, quoteID)
		return err
	})
	return out, err
}

func quoteOrder(q c.Quote, id, customer, key string, labels map[string]string) (oc.CreateInput, error) {
	invalid := func() (oc.CreateInput, error) { return oc.CreateInput{}, c.ErrInvalid }
	if (q.Status != "approved" && q.Status != "sent" && q.Status != "accepted_pending_approval") || q.Version < 1 || !c.ValidID(q.ID) || !c.ValidID(id) || !c.ValidID(customer) || q.Currency != q.Snapshot.Currency || len(q.Snapshot.Lines) == 0 || q.Snapshot.Total <= 0 {
		return invalid()
	}
	out := oc.CreateInput{ID: id, IdempotencyKey: key, Buyer: oc.Buyer{CustomerID: customer}, Currency: q.Currency, Shipping: q.Snapshot.Shipping, ReservationTTLSeconds: 900}
	net, taxAmount, gross := money.Money{Currency: q.Currency}, money.Money{Currency: q.Currency}, money.Money{Currency: q.Currency}
	seen := make(map[int32]bool)
	for _, line := range q.Snapshot.Lines {
		if !c.ValidID(line.VariantID) || !c.ValidID(line.ProductID) || line.Quantity <= 0 || line.LineNo <= 0 || seen[line.LineNo] || labels[line.VariantID] == "" {
			return invalid()
		}
		seen[line.LineNo] = true
		unit := money.Money{Amount: line.UnitPrice, Currency: q.Currency}
		basis, err := unit.Mul(line.Quantity)
		if err != nil || unit.Amount < 0 {
			return invalid()
		}
		result := line.Snapshot
		source := result.Amounts.Net
		if result.Snapshot.IncludeTax {
			source = result.Amounts.Gross
		}
		if basis.Amount != source || result.Amounts.Net < 0 || result.Amounts.Tax < 0 || result.Amounts.Gross < 0 || result.Amounts.Net > result.Amounts.Gross || result.Amounts.Tax != result.Amounts.Gross-result.Amounts.Net {
			return invalid()
		}
		lineID := uuid.NewSHA1(uuid.MustParse(q.ID), []byte(fmt.Sprintf("order-line:%d", line.LineNo))).String()
		result.ID = lineID
		out.Lines = append(out.Lines, oc.Line{ID: lineID, SKUID: line.VariantID, SKU: labels[line.VariantID], Quantity: line.Quantity, Price: pricing.Result{Unit: unit, Line: basis}, Tax: result, QuoteSource: &oc.QuoteSource{QuoteID: q.ID, Version: q.Version, LineNo: line.LineNo}})
		net, err = net.Add(money.Money{Amount: result.Amounts.Net, Currency: q.Currency})
		if err != nil {
			return invalid()
		}
		taxAmount, err = taxAmount.Add(money.Money{Amount: result.Amounts.Tax, Currency: q.Currency})
		if err != nil {
			return invalid()
		}
		gross, err = gross.Add(money.Money{Amount: result.Amounts.Gross, Currency: q.Currency})
		if err != nil {
			return invalid()
		}
	}
	total, err := gross.Add(q.Snapshot.Shipping.Charge)
	if err != nil || net.Amount != q.Snapshot.Subtotal || taxAmount.Amount != q.Snapshot.TaxAmount || q.Snapshot.Shipping.Charge.Amount != q.Snapshot.ShippingAmount || total.Amount != q.Snapshot.Total {
		return invalid()
	}
	return out, nil
}

func conversionOrderError(err error) error {
	switch {
	case errors.Is(err, oc.ErrNotFound):
		return c.ErrNotFound
	case errors.Is(err, oc.ErrInvalid):
		return c.ErrInvalid
	case errors.Is(err, oc.ErrKeyReused):
		return c.ErrIdempotencyReused
	case errors.Is(err, oc.ErrStockUnavailable), errors.Is(err, oc.ErrConflict), errors.Is(err, oc.ErrReservationOwner), errors.Is(err, oc.ErrRetry):
		return c.ErrConflict
	default:
		return c.ErrUnavailable
	}
}
