// Responsibility: pin the typed adopter boundary and checkout key namespace.
// Dependencies: public contracts only; production PG gates cover persistence.
// Invariant: raw quote keys stay unchanged; prepayment only adopts, never confirms.
package bootstrap

import (
	"context"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	ch "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"testing"
)

type quoteAdopterProbe struct {
	key, order                             string
	adopted, started, confirmed, cancelled int
}
type quoteServiceOnly struct{ ch.Service }

func (p *quoteAdopterProbe) Start(context.Context, ch.AuthorizedActor, ch.StartInput) (ch.Result, error) {
	p.started++
	return ch.Result{}, ch.ErrUnavailable
}
func (p *quoteAdopterProbe) Confirm(context.Context, payment.VerifiedEvent) (ch.Result, error) {
	p.confirmed++
	return ch.Result{}, ch.ErrUnavailable
}
func (p *quoteAdopterProbe) Cancel(context.Context, ch.AuthorizedActor, ch.CancelInput) (ch.Result, error) {
	p.cancelled++
	return ch.Result{}, ch.ErrUnavailable
}
func (p *quoteAdopterProbe) AdoptOrder(_ context.Context, a ch.AuthorizedActor, order, key string) (ch.Result, error) {
	p.adopted++
	p.key = key
	p.order = order
	if a.Kind() != "customer" {
		return ch.Result{}, ch.ErrForbidden
	}
	return ch.Result{OrderID: order, AmountMinor: 270, Currency: "USD", State: "ready", Payment: ch.Intent{ProviderRef: "fake-probe-intent"}}, nil
}

func TestQuoteCheckoutUsesImmutableOrderNamespaceAndAdoptionOnly(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const buyer = "22222222-2222-4222-8222-222222222222"
	const company = "33333333-3333-4333-8333-333333333333"
	const order = "44444444-4444-4444-8444-444444444444"
	actor, err := qc.ActorFromContext(qc.WithTrustedActor(context.Background(), qc.ActorIdentity{TenantID: tenant, CustomerID: buyer, CompanyID: company}))
	if err != nil {
		t.Fatal(err)
	}
	p := &quoteAdopterProbe{}
	receipt := qc.Conversion{OrderID: order, CompanyID: company, CustomerID: buyer, IdempotencyKey: "shared-cart-and-quote-raw-key", Amount: money.Money{Amount: 270, Currency: "USD"}}
	for i := 0; i < 2; i++ {
		out, err := (quoteCheckout{p}).Prepay(context.Background(), actor, receipt)
		if err != nil || out.State != "pending_payment" || out.ProviderRef == "" {
			t.Fatalf("adoption: %+v %v", out, err)
		}
		if p.key != "quotes.orders.v1:"+order || p.key == receipt.IdempotencyKey || p.order != order {
			t.Fatal("checkout shares raw cart/quote key")
		}
	}
	if receipt.IdempotencyKey != "shared-cart-and-quote-raw-key" || p.adopted != 2 || p.started != 0 || p.confirmed != 0 || p.cancelled != 0 {
		t.Fatal("raw receipt or adoption-only boundary changed")
	}
	if _, err := NewQuoteConversion(QuoteConversionConfig{}, p); err == nil {
		t.Fatal("incomplete typed composition accepted")
	}
	config := QuoteConversionConfig{Runner: runtimeQuoteRunner{}, Quotes: quotes.New(), Orders: orders.NewQuoteCreator()}
	if _, err := NewQuoteConversion(config, quoteServiceOnly{p}); err == nil {
		t.Fatal("Service without existing-order adoption accepted")
	}
	if _, err := New(Config{QuoteConversion: &config}); err == nil {
		t.Fatal("typed buyer composition without capabilities accepted")
	}
}
