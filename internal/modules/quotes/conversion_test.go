// Responsibility: immutable quote projection and trusted conversion admission.
// Dependencies: public quote/order contracts and money; callers: Go gates.
// Invariants: no price-list fiction, live repricing or free-form SKU substitution.
package quotes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

func conversionQuote() c.Quote {
	id, tenant, company, variant, product := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	line := c.Line{LineNo: 1, ProductID: product, VariantID: variant, Quantity: 2, UnitPrice: 100,
		Snapshot: tax.LineResult{ID: "1", Amounts: tax.Amounts{Net: 200, Tax: 20, Gross: 220}, Snapshot: tax.Snapshot{Currency: "USD", Category: "standard", Rounding: "half_up", Reason: "rate"}}}
	return c.Quote{ID: id, TenantID: tenant, CompanyID: company, Currency: "USD", Status: "approved", Version: 4,
		Snapshot: c.Snapshot{Currency: "USD", Lines: []c.Line{line}, Subtotal: 200, TaxAmount: 20, ShippingAmount: 50, Total: 270,
			Shipping: shipping.Result{Charge: money.Money{Amount: 50, Currency: "USD"}}}}
}

func TestConversionProjectionKeepsNegotiatedSnapshot(t *testing.T) {
	q := conversionQuote()
	id, customer := uuid.NewString(), uuid.NewString()
	in, err := quoteOrder(q, id, customer, "once", map[string]string{q.Snapshot.Lines[0].VariantID: "ACTUAL-SKU"})
	if err != nil {
		t.Fatal(err)
	}
	l := in.Lines[0]
	if l.SKUID != q.Snapshot.Lines[0].VariantID || l.SKU != "ACTUAL-SKU" || l.Quantity != 2 || l.Price.Unit.Amount != 100 || l.Price.Line.Amount != 200 || l.Price.PriceListID != "" || l.QuoteSource == nil || l.QuoteSource.QuoteID != q.ID || l.QuoteSource.Version != 4 || l.Tax.Amounts != q.Snapshot.Lines[0].Snapshot.Amounts || l.Tax.Snapshot != q.Snapshot.Lines[0].Snapshot.Snapshot || in.Shipping != q.Snapshot.Shipping || in.Buyer.CustomerID != customer {
		t.Fatalf("negotiated snapshot changed: %+v", in)
	}
}

func TestConversionProjectionRejectsUnfulfillableAndUnsafeQuotes(t *testing.T) {
	for _, mutation := range []func(*c.Quote){
		func(q *c.Quote) { q.Snapshot.Lines[0].VariantID = "" },
		func(q *c.Quote) { q.Status = "sent" },
		func(q *c.Quote) { q.Snapshot.Total++ },
		func(q *c.Quote) { q.Snapshot.Currency = "EUR" },
		func(q *c.Quote) { q.Snapshot.Lines[0].Quantity = 0 },
	} {
		q := conversionQuote()
		mutation(&q)
		if _, err := quoteOrder(q, uuid.NewString(), uuid.NewString(), "once", map[string]string{q.Snapshot.Lines[0].VariantID: "ACTUAL-SKU"}); !errors.Is(err, c.ErrInvalid) {
			t.Fatalf("unsafe quote accepted: %v", err)
		}
	}
}

func TestConversionDefaultDeniesAuthorityAndMissingCheckout(t *testing.T) {
	a := NewConversionApplication(ConversionConfig{})
	if _, err := a.Convert(context.Background(), uuid.NewString(), "once"); !errors.Is(err, c.ErrUnauthorized) {
		t.Fatalf("anonymous conversion: %v", err)
	}
	ctx := c.WithTrustedActor(context.Background(), c.ActorIdentity{TenantID: uuid.NewString(), CustomerID: uuid.NewString(), CompanyID: uuid.NewString()})
	if _, err := a.Convert(ctx, uuid.NewString(), "once"); !errors.Is(err, c.ErrUnavailable) {
		t.Fatalf("missing checkout: %v", err)
	}
	staff := c.WithTrustedActor(context.Background(), c.ActorIdentity{TenantID: uuid.NewString(), StaffID: uuid.NewString()})
	if _, err := a.Convert(staff, uuid.NewString(), "once"); !errors.Is(err, c.ErrForbidden) {
		t.Fatalf("staff used buyer endpoint: %v", err)
	}
}

type conversionRunner struct{}

func (conversionRunner) InTenant(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
	return fn(ctx, nil)
}

type conversionRepository struct {
	quote   c.Quote
	receipt c.Conversion
	denied  bool
}

func (r *conversionRepository) LockForConversion(context.Context, pgx.Tx, c.AuthorizedActor, string) (c.Quote, error) {
	if r.denied {
		return c.Quote{}, c.ErrNotFound
	}
	return r.quote, nil
}
func (r *conversionRepository) GetConversion(context.Context, pgx.Tx, c.AuthorizedActor, string) (c.Conversion, error) {
	if r.denied || r.receipt.OrderID == "" {
		return c.Conversion{}, c.ErrNotFound
	}
	return r.receipt, nil
}
func (r *conversionRepository) ConversionSKU(context.Context, pgx.Tx, c.AuthorizedActor, string, string) (string, error) {
	return "ACTUAL-SKU", nil
}
func (r *conversionRepository) BindConversion(_ context.Context, _ pgx.Tx, a c.AuthorizedActor, q, order string, version int64, key string, _ []byte) (c.Conversion, error) {
	r.receipt = c.Conversion{QuoteID: q, OrderID: order, CompanyID: a.CompanyID(), CustomerID: a.ID(), IdempotencyKey: key, QuoteVersion: version, OrderNumber: 1, Amount: money.Money{Amount: r.quote.Snapshot.Total, Currency: r.quote.Currency}, State: "pending_payment", CreatedAt: time.Now()}
	return r.receipt, nil
}

type conversionCreator struct{ calls int }

func (o *conversionCreator) CreateFromQuote(_ context.Context, _ pgx.Tx, tenant string, in oc.CreateInput) (oc.Order, error) {
	o.calls++
	return oc.Order{ID: in.ID, TenantID: tenant}, nil
}

type conversionPrepay struct {
	calls    int
	fakePaid bool
}

func (p *conversionPrepay) Prepay(_ context.Context, _ c.AuthorizedActor, r c.Conversion) (c.Prepayment, error) {
	p.calls++
	state := r.State
	if p.fakePaid {
		state = "paid"
	}
	return c.Prepayment{OrderID: r.OrderID, Amount: r.Amount, ProviderRef: "fixture", State: state}, nil
}

func TestConversionReplayAndProviderResultCannotForgePaid(t *testing.T) {
	q := conversionQuote()
	customer := uuid.NewString()
	ctx := c.WithTrustedActor(context.Background(), c.ActorIdentity{TenantID: q.TenantID, CustomerID: customer, CompanyID: q.CompanyID})
	repo, creator, prepay := &conversionRepository{quote: q}, &conversionCreator{}, &conversionPrepay{}
	a := NewConversionApplication(ConversionConfig{Runner: conversionRunner{}, Quotes: repo, Orders: creator, Checkout: prepay})
	first, err := a.Convert(ctx, q.ID, "once")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Convert(ctx, q.ID, "once")
	if err != nil || second.OrderID != first.OrderID || creator.calls != 1 {
		t.Fatalf("replay duplicated order: %+v, %v, creates=%d", second, err, creator.calls)
	}
	if _, err = a.Convert(ctx, q.ID, "different"); !errors.Is(err, c.ErrConflict) {
		t.Fatalf("second key created again: %v", err)
	}
	prepay.fakePaid = true
	if _, err = a.Convert(ctx, q.ID, "once"); !errors.Is(err, c.ErrUnavailable) {
		t.Fatalf("provider forged paid without DB transition: %v", err)
	}
	repo.denied = true
	before := prepay.calls
	if _, err = a.Convert(ctx, q.ID, "once"); !errors.Is(err, c.ErrNotFound) || prepay.calls != before {
		t.Fatalf("revoked replay reached payment port: %v", err)
	}
}

// A completed conversion is durable history. Fulfillment/refunds must not restart
// payment or turn a same-key replay into a provider-state error.
func TestConversionHistoricalReplayDoesNotRestartPayment(t *testing.T) {
	for _, state := range []string{"paid", "cancelled", "partially_fulfilled", "fulfilled", "partially_refunded", "refunded"} {
		t.Run(state, func(t *testing.T) {
			q := conversionQuote()
			customer := uuid.NewString()
			ctx := c.WithTrustedActor(context.Background(), c.ActorIdentity{TenantID: q.TenantID, CustomerID: customer, CompanyID: q.CompanyID})
			saved := c.Conversion{QuoteID: q.ID, OrderID: uuid.NewString(), CompanyID: q.CompanyID, CustomerID: customer, IdempotencyKey: "history", QuoteVersion: q.Version, OrderNumber: 1, Amount: money.Money{Currency: q.Currency, Amount: q.Snapshot.Total}, State: state, CreatedAt: time.Now()}
			repo, creator, prepay := &conversionRepository{quote: q, receipt: saved}, &conversionCreator{}, &conversionPrepay{fakePaid: true}
			a := NewConversionApplication(ConversionConfig{Runner: conversionRunner{}, Quotes: repo, Orders: creator, Checkout: prepay})
			got, err := a.Convert(ctx, q.ID, "history")
			if err != nil || got.OrderID != saved.OrderID || got.State != state || got.Amount != saved.Amount || creator.calls != 0 || prepay.calls != 0 {
				t.Fatalf("CONVERSION_HISTORICAL_REPLAY restarted payment or lost durable state: state=%s got=%+v err=%v creates=%d prepay=%d", state, got, err, creator.calls, prepay.calls)
			}
		})
	}
}
