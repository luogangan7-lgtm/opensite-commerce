//go:build pgx

// Responsibility: exercise approved quote -> real checkout -> trusted Confirm in PG.
// Dependencies: bootstrap typed composition and public modules; pgxadapter owns pools.
// Invariants: expendable fixtures, effective app_runtime, provider IO at tx depth zero;
// tests never implement order transitions, captures or business journals themselves.
package bootstrap_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type quoteOrderDepth struct{ depth int }
type quoteOrderDepthKey struct{}
type quoteOrderRunner struct{ pgxadapter.Runner }

func (r quoteOrderRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if marker, ok := ctx.Value(quoteOrderDepthKey{}).(*quoteOrderDepth); ok {
		marker.depth++
		defer func() { marker.depth-- }()
	}
	return r.Runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

type quoteOrderFixture struct {
	ctx                                                                                                    context.Context
	runner                                                                                                 quoteOrderRunner
	tenant, site, staff, company, buyer, approver, otherCompany, profile, inquiry, product, sku, priceList string
	accounts                                                                                               map[string]string
	actor                                                                                                  qc.AuthorizedActor
	buyerCtx                                                                                               context.Context
	quote                                                                                                  qc.Quote
	fake                                                                                                   *paymentfake.Fake
	prepay                                                                                                 *quoteOrderPaymentProbe
	checkout                                                                                               checkoutcontract.Service
	confirm                                                                                                http.Handler
	signingKey                                                                                             []byte
	app                                                                                                    *quotes.ConversionApplication
}

func quoteOrdersDB(t *testing.T, expiry time.Duration) *quoteOrderFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		pgxtest.Skip(t, "NOT_RUN: mandatory expendable migrated PostgreSQL DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	r, close, err := pgxadapter.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	f := &quoteOrderFixture{ctx: ctx, runner: quoteOrderRunner{r}, tenant: uuid.NewString(), site: uuid.NewString(), staff: uuid.NewString(), company: uuid.NewString(), buyer: uuid.NewString(), approver: uuid.NewString(), otherCompany: uuid.NewString(), profile: uuid.NewString(), product: uuid.NewString(), sku: uuid.NewString(), priceList: uuid.NewString(), accounts: map[string]string{}}
	org, zone, taxzone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		steps := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO app.tenants(id,name,settlement_currency) VALUES($1,'quote-orders-gate','USD')`, []any{f.tenant}},
			{`INSERT INTO app.users(id,email,password_hash) VALUES($1,$2,'fixture-hash')`, []any{f.staff, f.staff + "@example.test"}},
			{`INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'admin')`, []any{f.tenant, f.staff}},
			{`INSERT INTO app.organizations(tenant_id,id,display_name) VALUES($1,$2,'quote-orders')`, []any{f.tenant, org}},
			{`INSERT INTO app.sites(tenant_id,id,organization_id,name) VALUES($1,$2,$3,'quote-orders')`, []any{f.tenant, f.site, org}},
			{`INSERT INTO app.buyer_companies(tenant_id,id,name) VALUES($1,$2,'buyer'),($1,$3,'other')`, []any{f.tenant, f.company, f.otherCompany}},
			{`INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,$4),($1,$3,$5)`, []any{f.tenant, f.buyer, f.approver, f.buyer + "@example.test", f.approver + "@example.test"}},
			{`INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES($1,$2,$3,'admin'),($1,$2,$4,'approver'),($1,$5,$3,'purchaser')`, []any{f.tenant, f.company, f.buyer, f.approver, f.otherCompany}},
			{`INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES($1,$2,'quote-orders','ready')`, []any{f.tenant, f.product}},
			{`INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES($1,$2,$3,'QUOTE-ORDER-SKU','default','ready')`, []any{f.tenant, f.sku, f.product}},
			{`INSERT INTO app.tax_settings(tenant_id,prices_include_tax) VALUES($1,false)`, []any{f.tenant}},
			{`INSERT INTO app.tax_zones(tenant_id,id,name) VALUES($1,$2,'US')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.tax_categories(tenant_id,code,name) VALUES($1,'standard','Standard')`, []any{f.tenant}},
			{`INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES($1,$2,'standard',1000,clock_timestamp()-interval '1 hour')`, []any{f.tenant, taxzone}},
			{`INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'US')`, []any{f.tenant, zone}},
			{`INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')`, []any{f.tenant, zone}},
			{`INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES($1,$2,'Standard','USD')`, []any{f.tenant, f.profile}},
			{`INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',50)`, []any{f.tenant, f.profile, zone}},
			{`INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at) VALUES($1,$2,'USD','active',clock_timestamp()-interval '1 hour')`, []any{f.tenant, f.priceList}},
			{`INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES($1,$2,$3,1,500)`, []any{f.tenant, f.priceList, f.sku}},
			{`SELECT set_config('app.tenant_id',$1,true)`, []any{f.tenant}},
			{`SELECT app.inventory_adjust($1,100,'quote-orders fixture',$2,'inbound')`, []any{f.sku, uuid.NewString()}},
		}
		for i, s := range steps {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return fmt.Errorf("quote-order fixture %d: %w", i, err)
			}
		}
		for _, a := range []struct{ code, kind string }{{"accounts_receivable", "asset"}, {"revenue", "revenue"}, {"tax_payable", "liability"}, {"shipping_revenue", "revenue"}, {"cash", "asset"}} {
			id := uuid.NewString()
			f.accounts[a.code] = id
			if _, err := tx.Exec(ctx, `INSERT INTO app.ledger_accounts(tenant_id,id,currency,code,account_type) VALUES($1,$2,'USD',$3,$4)`, f.tenant, id, a.code, a.kind); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.buyerCtx = qc.WithTrustedActor(ctx, qc.ActorIdentity{TenantID: f.tenant, CustomerID: f.buyer, CompanyID: f.company})
	f.actor, err = qc.ActorFromContext(f.buyerCtx)
	if err != nil {
		t.Fatal(err)
	}
	staff := f.mustActor(t, f.staff, "", true)
	approver := f.mustActor(t, f.approver, f.company, false)
	m := quotes.New()
	err = f.run(func(ctx context.Context, tx pgx.Tx) error {
		in, err := m.SubmitInquiry(ctx, tx, f.tenant, qc.InquiryInput{SiteID: f.site, Email: f.buyer + "@example.test", Name: "Buyer", Message: "negotiated order", ProductID: f.product})
		f.inquiry = in.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Public inquiry lacks company binding today. Test provisioning supplies this
	// explicit association; this is not buyer authorization or a production route.
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.inquiries SET buyer_company_id=$3 WHERE tenant_id=$1 AND id=$2`, f.tenant, f.inquiry, f.company)
		return err
	})
	in := qc.CreateInput{SiteID: f.site, InquiryID: f.inquiry, CompanyID: f.company, Currency: "USD", ExpiresAt: time.Now().Add(expiry), Lines: []qc.LineInput{{ProductID: f.product, VariantID: f.sku, Description: "negotiated", Quantity: 2, UnitPrice: 100, TaxCategory: "standard"}}, Destination: tax.Destination{Country: "US"}, ShippingProfileID: f.profile}
	err = f.run(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		f.quote, e = m.Create(ctx, tx, staff, in, qc.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	transition := func(actor qc.AuthorizedActor, action qc.Action) error {
		return f.run(func(ctx context.Context, tx pgx.Tx) error {
			out, e := m.Transition(ctx, tx, actor, f.quote.ID, action, qc.WriteOptions{IfMatch: f.quote.ETag(), IdempotencyKey: uuid.NewString()})
			if e == nil {
				f.quote = out
			}
			return e
		})
	}
	if err := transition(staff, qc.Send); err != nil {
		t.Fatal(err)
	}
	if err := transition(f.actor, qc.Accept); err != nil {
		t.Fatal(err)
	}
	if err := transition(f.actor, qc.Approve); !errors.Is(err, qc.ErrForbidden) {
		t.Fatalf("self approval: %v", err)
	}
	if err := transition(approver, qc.Approve); err != nil {
		t.Fatal(err)
	}
	if f.quote.Version != 4 || f.quote.Snapshot.Total != 270 {
		t.Fatalf("approved snapshot: %+v", f.quote)
	}
	f.fake = paymentfake.New()
	f.prepay = &quoteOrderPaymentProbe{fake: f.fake}
	f.checkout = checkout.New(checkout.Config{Run: f.runner.InTenant, Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: f.prepay})
	f.app, err = bootstrap.NewQuoteConversion(bootstrap.QuoteConversionConfig{Runner: f.runner, Quotes: m, Orders: orders.NewQuoteCreator()}, f.checkout)
	if err != nil {
		t.Fatal(err)
	}
	f.signingKey = make([]byte, 32)
	if _, err := rand.Read(f.signingKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.prepay.violations != 0 {
			t.Errorf("provider IO in transactions: %d", f.prepay.violations)
		}
	})
	return f
}
func (f *quoteOrderFixture) mustActor(t *testing.T, id, company string, staff bool) qc.AuthorizedActor {
	t.Helper()
	identity := qc.ActorIdentity{TenantID: f.tenant, CompanyID: company}
	if staff {
		identity.StaffID = id
	} else {
		identity.CustomerID = id
	}
	a, e := qc.ActorFromContext(qc.WithTrustedActor(f.ctx, identity))
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func (f *quoteOrderFixture) run(fn func(context.Context, pgx.Tx) error) error {
	return f.runner.InTenant(f.ctx, f.tenant, fn)
}
func (f *quoteOrderFixture) maintenance(t *testing.T, fn func(context.Context, pgx.Tx) error) {
	t.Helper()
	if e := f.runner.InMaintenance(f.ctx, fn); e != nil {
		t.Fatal(e)
	}
}

// The neutral probe observes provider calls; it implements no business protocol.
type quoteOrderPaymentProbe struct {
	mu                                                 sync.Mutex
	fake                                               *paymentfake.Fake
	creates                                            []payment.CreatePaymentIntent
	createCalls, captureCalls, cancelCalls, violations int
}

func (p *quoteOrderPaymentProbe) check(ctx context.Context, op paymentfake.Operation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if marker, ok := ctx.Value(quoteOrderDepthKey{}).(*quoteOrderDepth); !ok || marker.depth != 0 {
		p.violations++
		return errors.New("provider IO requires observed transaction depth zero")
	}
	switch op {
	case paymentfake.Create:
		p.createCalls++
	case paymentfake.Capture:
		p.captureCalls++
	case paymentfake.Cancel:
		p.cancelCalls++
	}
	return nil
}
func (p *quoteOrderPaymentProbe) CreateIntent(ctx context.Context, in payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Create); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	p.mu.Lock()
	p.creates = append(p.creates, in)
	p.mu.Unlock()
	return p.fake.CreateIntent(ctx, in)
}
func (p *quoteOrderPaymentProbe) Capture(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Capture); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return p.fake.Capture(ctx, in)
}
func (p *quoteOrderPaymentProbe) Cancel(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Cancel); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return p.fake.Cancel(ctx, in)
}
func (p *quoteOrderPaymentProbe) Refund(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Refund); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return p.fake.Refund(ctx, in)
}

func quoteObserved(ctx context.Context) context.Context {
	return context.WithValue(ctx, quoteOrderDepthKey{}, &quoteOrderDepth{})
}

// Exercise the trusted private command, including provider capture and recovery.
// Public callbacks consume completed facts separately in deliverFact.
func (f *quoteOrderFixture) deliver(t *testing.T, orderID string) *httptest.ResponseRecorder {
	t.Helper()
	for _, event := range f.fake.Events() {
		if event.OrderID != orderID || event.Status != payment.StatusAuthorized {
			continue
		}
		_, err := f.checkout.Confirm(quoteObserved(f.ctx), event.VerifiedEvent)
		response := httptest.NewRecorder()
		if err != nil {
			var providerError *payment.PaymentError
			retryable := errors.As(err, &providerError) && providerError.Code == payment.ErrorRetryable
			if !retryable && !errors.Is(err, checkoutcontract.ErrRetry) && !errors.Is(err, checkoutcontract.ErrUnavailable) {
				t.Fatalf("trusted Confirm unexpected error: %v", err)
			}
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(response, err)
		} else {
			response.WriteHeader(http.StatusOK)
		}
		return response
	}
	t.Fatal("actual provider authorized event absent")
	return nil
}

func (f *quoteOrderFixture) convertAndPay(t *testing.T, ctx context.Context, quoteID, key string) (qc.Conversion, error) {
	t.Helper()
	captures := f.prepay.captureCalls
	out, err := f.app.Convert(quoteObserved(ctx), quoteID, key)
	if err != nil || out.State != string(oc.PendingPayment) {
		return out, err
	}
	if f.prepay.captureCalls != captures {
		t.Fatal("conversion captured before trusted Confirm")
	}
	if out.ProviderRef == "" {
		t.Fatal("conversion did not adopt a pending provider intent")
	}
	response := f.deliver(t, out.OrderID)
	if response.Code != http.StatusOK {
		t.Fatalf("trusted Confirm: %d %s", response.Code, response.Body.String())
	}
	return f.app.Convert(quoteObserved(ctx), quoteID, key)
}

func (f *quoteOrderFixture) assertEffects(t *testing.T, order, state string, ordersCount, journals, paidOutbox, onHand, reserved int64) {
	t.Helper()
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var n, conv, j, paid, hand, held int64
		var gotState string
		e := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.orders WHERE tenant_id=$1),(SELECT count(*) FROM app.quote_conversions WHERE tenant_id=$1),(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1),(SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND event_type='order.paid'),(SELECT on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2),(SELECT reserved FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2)`, f.tenant, f.sku).Scan(&n, &conv, &j, &paid, &hand, &held)
		if e != nil {
			return e
		}
		if n != ordersCount || conv != ordersCount || j != journals || paid != paidOutbox || hand != onHand || held != reserved {
			return fmt.Errorf("effects orders=%d conversions=%d journals=%d paidOutbox=%d onHand=%d held=%d", n, conv, j, paid, hand, held)
		}
		if order != "" {
			if e := tx.QueryRow(ctx, `SELECT state FROM app.orders WHERE tenant_id=$1 AND id=$2`, f.tenant, order).Scan(&gotState); e != nil {
				return e
			}
			if gotState != state {
				return fmt.Errorf("state=%s want=%s", gotState, state)
			}
			reservationState := map[string]string{"pending_payment": "active", "paid": "committed", "cancelled": "released"}[state]
			var reservationCount, matchingReservations, createdEvents, convertedEvents, cancelledEvents int64
			if e := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE state=$3) FROM app.inventory_reservations WHERE tenant_id=$1 AND owner_id=$2`, f.tenant, order, reservationState).Scan(&reservationCount, &matchingReservations); e != nil {
				return e
			}
			if reservationCount != 1 || matchingReservations != 1 {
				return fmt.Errorf("reservation state mismatch count=%d matching=%d want=%s", reservationCount, matchingReservations, reservationState)
			}
			if e := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE event_type='order.created'),count(*) FILTER(WHERE event_type='quote.converted'),count(*) FILTER(WHERE event_type='order.cancelled') FROM app.outbox_events WHERE tenant_id=$1`, f.tenant).Scan(&createdEvents, &convertedEvents, &cancelledEvents); e != nil {
				return e
			}
			wantCancel := int64(0)
			if state == "cancelled" {
				wantCancel = 1
			}
			if createdEvents != ordersCount || convertedEvents != ordersCount || cancelledEvents != wantCancel {
				return fmt.Errorf("outbox created=%d converted=%d cancelled=%d", createdEvents, convertedEvents, cancelledEvents)
			}
		}
		if journals > 0 {
			var debit, credit int64
			if e := tx.QueryRow(ctx, `SELECT coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint FROM app.ledger_entries WHERE tenant_id=$1`, f.tenant).Scan(&debit, &credit); e != nil {
				return e
			}
			if debit != 540*journals || credit != 540*journals {
				return fmt.Errorf("journal balance %d/%d", debit, credit)
			}
			var exact bool
			if e := tx.QueryRow(ctx, `WITH actual AS (SELECT a.code,e.side,sum(e.amount)::bigint AS amount FROM app.ledger_entries e JOIN app.ledger_accounts a ON a.tenant_id=e.tenant_id AND a.id=e.account_id WHERE e.tenant_id=$1 GROUP BY a.code,e.side), expected(code,side,amount) AS (VALUES('accounts_receivable','debit',270::bigint*$2),('accounts_receivable','credit',270::bigint*$2),('cash','debit',270::bigint*$2),('revenue','credit',200::bigint*$2),('tax_payable','credit',20::bigint*$2),('shipping_revenue','credit',50::bigint*$2)) SELECT NOT EXISTS((SELECT * FROM actual EXCEPT ALL SELECT * FROM expected) UNION ALL (SELECT * FROM expected EXCEPT ALL SELECT * FROM actual))`, f.tenant, journals).Scan(&exact); e != nil {
				return e
			}
			if !exact {
				return errors.New("ledger did not use B2C account meanings and frozen amounts")
			}
		} else {
			var entries int64
			if e := tx.QueryRow(ctx, `SELECT count(*) FROM app.ledger_entries WHERE tenant_id=$1`, f.tenant).Scan(&entries); e != nil {
				return e
			}
			if entries != 0 {
				return fmt.Errorf("unpaid journal entries=%d", entries)
			}
		}
		return nil
	})
}

func TestQuoteOrdersPGApprovedSnapshotAndReplay(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{`UPDATE app.price_entries SET unit_amount=999 WHERE tenant_id=$1`, `UPDATE app.tax_rates SET rate_bp=2000 WHERE tenant_id=$1`, `UPDATE app.shipping_rates SET amount=99 WHERE tenant_id=$1`} {
			if _, e := tx.Exec(ctx, s, f.tenant); e != nil {
				return e
			}
		}
		return nil
	})
	key := uuid.NewString()
	first, e := f.convertAndPay(t, f.buyerCtx, f.quote.ID, key)
	if e != nil {
		t.Fatal(e)
	}
	if first.State != string(oc.Paid) || first.Amount != (money.Money{Currency: "USD", Amount: 270}) || first.ProviderRef == "" {
		t.Fatalf("conversion %+v", first)
	}
	second, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
	if e != nil || second.OrderID != first.OrderID || second.State != first.State || second.Amount != first.Amount {
		t.Fatalf("replay %+v %v", second, e)
	}
	if _, e := f.app.Convert(f.buyerCtx, f.quote.ID, uuid.NewString()); !errors.Is(e, qc.ErrConflict) {
		t.Fatalf("different conversion key %v", e)
	}
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 1 {
		t.Fatalf("provider calls %d/%d", f.prepay.createCalls, f.prepay.captureCalls)
	}
	f.assertEffects(t, first.OrderID, "paid", 1, 1, 1, 98, 0)
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var unit, taxMinor, total int64
		var ship int64
		e := tx.QueryRow(ctx, `SELECT l.unit_price_minor,l.line_tax_minor,o.total_minor,o.shipping_minor FROM app.order_lines l JOIN app.orders o ON o.tenant_id=l.tenant_id AND o.id=l.order_id WHERE o.tenant_id=$1 AND o.id=$2`, f.tenant, first.OrderID).Scan(&unit, &taxMinor, &total, &ship)
		if e != nil {
			return e
		}
		if unit != 100 || taxMinor != 20 || total != 270 || ship != 50 {
			return fmt.Errorf("repriced snapshot %d/%d/%d/%d", unit, taxMinor, total, ship)
		}
		return nil
	})
}

// A quote receipt key is scoped to the buyer; the Orders creation key must not
// widen it to a tenant-wide collision. Both buyers use the real public ports.
func TestQuoteOrdersPGCreationKeyIsBuyerScoped(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	m := quotes.New()
	approveQuote := func(buyer, approver qc.AuthorizedActor) qc.Quote {
		t.Helper()
		var q qc.Quote
		staff := f.mustActor(t, f.staff, "", true)
		in := qc.CreateInput{SiteID: f.site, InquiryID: f.inquiry, CompanyID: f.company, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour), Lines: []qc.LineInput{{ProductID: f.product, VariantID: f.sku, Description: "buyer-scoped-key", Quantity: 2, UnitPrice: 100, TaxCategory: "standard"}}, Destination: tax.Destination{Country: "US"}, ShippingProfileID: f.profile}
		if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
			var e error
			q, e = m.Create(ctx, tx, staff, in, qc.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
			return e
		}); e != nil {
			t.Fatal(e)
		}
		for _, step := range []struct {
			actor  qc.AuthorizedActor
			action qc.Action
		}{{staff, qc.Send}, {buyer, qc.Accept}, {approver, qc.Approve}} {
			if e := f.run(func(ctx context.Context, tx pgx.Tx) error {
				var e error
				q, e = m.Transition(ctx, tx, step.actor, q.ID, step.action, qc.WriteOptions{IfMatch: q.ETag(), IdempotencyKey: uuid.NewString()})
				return e
			}); e != nil {
				t.Fatal(e)
			}
		}
		return q
	}
	// Existing fixture buyer is admin and the second customer is approver, so
	// neither quote is self-approved and both have current company membership.
	secondActor := f.mustActor(t, f.approver, f.company, false)
	secondCtx := qc.WithTrustedActor(f.ctx, qc.ActorIdentity{TenantID: f.tenant, CustomerID: f.approver, CompanyID: f.company})
	secondQuote := approveQuote(secondActor, f.actor)
	thirdQuote := approveQuote(f.actor, secondActor)
	key := "shared-customer-conversion-key"
	first, e := f.convertAndPay(t, f.buyerCtx, f.quote.ID, key)
	if e != nil {
		t.Fatalf("first buyer conversion: %v", e)
	}
	second, e := f.convertAndPay(t, secondCtx, secondQuote.ID, key)
	if e != nil {
		t.Fatalf("same raw key for another buyer must succeed: %v", e)
	}
	if first.OrderID == second.OrderID || first.CustomerID != f.buyer || second.CustomerID != f.approver || first.IdempotencyKey != key || second.IdempotencyKey != key {
		t.Fatalf("buyer key scope first=%+v second=%+v", first, second)
	}
	for _, item := range []struct {
		ctx   context.Context
		quote string
		want  qc.Conversion
	}{{f.buyerCtx, f.quote.ID, first}, {secondCtx, secondQuote.ID, second}} {
		got, e := f.app.Convert(item.ctx, item.quote, key)
		if e != nil || got.OrderID != item.want.OrderID || got.CustomerID != item.want.CustomerID || got.State != item.want.State || got.Amount != item.want.Amount || got.IdempotencyKey != key {
			t.Fatalf("buyer-scoped replay: %+v %v", got, e)
		}
	}
	f.assertEffects(t, first.OrderID, "paid", 2, 2, 2, 96, 0)
	if f.prepay.createCalls != 2 || f.prepay.captureCalls != 2 || f.prepay.cancelCalls != 0 {
		t.Fatalf("provider calls after two conversions/replays: %d/%d/%d", f.prepay.createCalls, f.prepay.captureCalls, f.prepay.cancelCalls)
	}
	if _, e := f.app.Convert(f.buyerCtx, thirdQuote.ID, key); !errors.Is(e, qc.ErrIdempotencyReused) {
		t.Fatalf("same buyer same key other quote must reject: %v", e)
	}
	f.assertEffects(t, second.OrderID, "paid", 2, 2, 2, 96, 0)
	if f.prepay.createCalls != 2 || f.prepay.captureCalls != 2 || f.prepay.cancelCalls != 0 {
		t.Fatalf("rejected key reuse invoked provider: %d/%d/%d", f.prepay.createCalls, f.prepay.captureCalls, f.prepay.cancelCalls)
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var linked, reservations, converted int64
		var thirdBound bool
		e := tx.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM app.quote_conversions c JOIN app.orders o ON (o.tenant_id,o.id)=(c.tenant_id,c.order_id) WHERE c.tenant_id=$1 AND c.customer_id=o.customer_id AND c.idempotency_key=$2),
		 (SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1),
		 (SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND event_type='quote.converted'),
		 EXISTS(SELECT FROM app.quote_conversions WHERE tenant_id=$1 AND quote_id=$3)`, f.tenant, key, thirdQuote.ID).Scan(&linked, &reservations, &converted, &thirdBound)
		if e != nil {
			return e
		}
		if linked != 2 || reservations != 2 || converted != 2 || thirdBound {
			return fmt.Errorf("buyer key scope effects linked=%d reservations=%d converted=%d thirdBound=%v", linked, reservations, converted, thirdBound)
		}
		return nil
	})
}

func TestQuoteOrdersPGConcurrentReplay(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	key := uuid.NewString()
	const n = 6
	start := make(chan struct{})
	out := make(chan qc.Conversion, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, e := f.app.Convert(quoteObserved(f.buyerCtx), f.quote.ID, key)
			out <- c
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for c := range out {
		if id == "" {
			id = c.OrderID
		}
		if id != c.OrderID || c.State != "pending_payment" {
			t.Fatalf("concurrent result %+v", c)
		}
	}
	if response := f.deliver(t, id); response.Code != http.StatusOK {
		t.Fatalf("concurrent trusted Confirm: %d %s", response.Code, response.Body.String())
	}
	if f.prepay.createCalls < 1 || f.prepay.captureCalls != 1 {
		t.Fatalf("provider transport create/capture %d/%d", f.prepay.createCalls, f.prepay.captureCalls)
	}
	for _, request := range f.prepay.creates {
		if request != f.prepay.creates[0] || request.OrderID != id || request.Amount.Amount != 270 || request.Amount.Currency != "USD" || request.IdempotencyKey == "" {
			t.Fatal("concurrent provider request binding changed")
		}
	}
	authorized := 0
	for _, event := range f.fake.Events() {
		if event.Status == payment.StatusAuthorized {
			authorized++
		}
	}
	if authorized != 1 {
		t.Fatalf("concurrent provider intent effects: %d", authorized)
	}
	if replay, err := f.app.Convert(f.buyerCtx, f.quote.ID, key); err != nil || replay.OrderID != id || replay.State != "paid" {
		t.Fatalf("concurrent durable replay: %+v %v", replay, err)
	}
	f.assertEffects(t, id, "paid", 1, 1, 1, 98, 0)
}

func TestQuoteOrdersPGReplayRechecksMembership(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	key := uuid.NewString()
	c, err := f.convertAndPay(t, f.buyerCtx, f.quote.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=$1 AND buyer_company_id=$2 AND customer_id=$3`, f.tenant, f.company, f.buyer)
		return e
	})
	if _, e := f.app.Convert(f.buyerCtx, f.quote.ID, key); !errors.Is(e, qc.ErrNotFound) {
		t.Fatalf("revoked conversion replay: %v", e)
	}
	if _, e := f.app.GetConversion(f.buyerCtx, f.quote.ID); !errors.Is(e, qc.ErrNotFound) {
		t.Fatalf("revoked conversion read: %v", e)
	}
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 1 {
		t.Fatal("revoked replay reached provider")
	}
	f.assertEffects(t, c.OrderID, "paid", 1, 1, 1, 98, 0)
}
func TestQuoteOrdersPGAuthorityAndExpiry(t *testing.T) {
	t.Run("authority", func(t *testing.T) {
		f := quoteOrdersDB(t, time.Hour)
		if _, e := f.app.Convert(f.ctx, f.quote.ID, uuid.NewString()); !errors.Is(e, qc.ErrUnauthorized) {
			t.Fatalf("missing actor %v", e)
		}
		for _, i := range []qc.ActorIdentity{{TenantID: f.tenant, CustomerID: f.buyer, CompanyID: f.otherCompany}, {TenantID: uuid.NewString(), CustomerID: f.buyer, CompanyID: f.company}} {
			if _, e := f.app.Convert(qc.WithTrustedActor(f.ctx, i), f.quote.ID, uuid.NewString()); !errors.Is(e, qc.ErrNotFound) {
				t.Fatalf("foreign scope %v", e)
			}
		}
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=$1 AND buyer_company_id=$2 AND customer_id=$3`, f.tenant, f.company, f.buyer)
			return e
		})
		if _, e := f.app.Convert(f.buyerCtx, f.quote.ID, uuid.NewString()); !errors.Is(e, qc.ErrNotFound) {
			t.Fatalf("revoked member %v", e)
		}
		if f.prepay.createCalls != 0 {
			t.Fatal("unauthorized intent")
		}
		f.assertEffects(t, "", "", 0, 0, 0, 100, 0)
	})
	t.Run("expiry", func(t *testing.T) {
		f := quoteOrdersDB(t, 3*time.Second)
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, f.quote.ExpiresAt)
			return e
		})
		if _, e := f.app.Convert(f.buyerCtx, f.quote.ID, uuid.NewString()); !errors.Is(e, qc.ErrConflict) {
			t.Fatalf("expired quote %v", e)
		}
		if f.prepay.createCalls != 0 {
			t.Fatal("expired quote intent")
		}
		f.assertEffects(t, "", "", 0, 0, 0, 100, 0)
	})
}
func TestQuoteOrdersPGDefinitiveFailureAndExpiredHold(t *testing.T) {
	for _, mode := range []string{"definitive", "expired-hold"} {
		t.Run(mode, func(t *testing.T) {
			f := quoteOrdersDB(t, time.Hour)
			if mode == "definitive" {
				f.fake.FailNext(paymentfake.Capture, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "definitive test decline"})
			}
			key := uuid.NewString()
			c, e := f.app.Convert(quoteObserved(f.buyerCtx), f.quote.ID, key)
			if e != nil {
				t.Fatal(e)
			}
			if c.State != "pending_payment" || f.prepay.captureCalls != 0 {
				t.Fatalf("conversion captured before event: %+v", c)
			}
			if mode == "expired-hold" {
				f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_id=$2`, f.tenant, c.OrderID)
					return e
				})
			}
			if response := f.deliver(t, c.OrderID); response.Code != http.StatusOK {
				t.Fatalf("failure trusted Confirm: %d %s", response.Code, response.Body.String())
			}
			c, e = f.app.Convert(f.buyerCtx, f.quote.ID, key)
			if e != nil {
				t.Fatal(e)
			}
			if c.State != "cancelled" {
				t.Fatalf("not cancelled %+v", c)
			}
			captures := 1
			if mode == "expired-hold" {
				captures = 0
			}
			if f.prepay.captureCalls != captures || f.prepay.cancelCalls != 1 {
				t.Fatalf("capture/cancel %d/%d", f.prepay.captureCalls, f.prepay.cancelCalls)
			}
			f.assertEffects(t, c.OrderID, "cancelled", 1, 0, 0, 100, 0)
			replay, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
			if e != nil || replay.OrderID != c.OrderID || replay.State != "cancelled" {
				t.Fatalf("cancel replay %+v %v", replay, e)
			}
			var canceled, captured int
			for _, event := range f.fake.Events() {
				if event.Status == payment.StatusCanceled {
					canceled++
				}
				if event.Status == payment.StatusCaptured {
					captured++
				}
			}
			if canceled != 1 || captured != 0 {
				t.Fatalf("Fake effects canceled=%d captured=%d", canceled, captured)
			}
		})
	}
}
func TestQuoteOrdersPGAmbiguousFailureRetainsReservation(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	f.fake.FailNext(paymentfake.Capture, &payment.PaymentError{Code: payment.ErrorRetryable, Message: "ambiguous test failure"})
	key := uuid.NewString()
	c, e := f.app.Convert(quoteObserved(f.buyerCtx), f.quote.ID, key)
	if e != nil || c.State != "pending_payment" || f.prepay.captureCalls != 0 {
		t.Fatalf("pending adoption %+v %v", c, e)
	}
	if response := f.deliver(t, c.OrderID); response.Code < 500 {
		t.Fatalf("ambiguous Confirm was accepted: %d %s", response.Code, response.Body.String())
	}
	if f.prepay.cancelCalls != 0 {
		t.Fatal("ambiguous failure canceled intent")
	}
	f.assertEffects(t, c.OrderID, "pending_payment", 1, 0, 0, 100, 2)
	if response := f.deliver(t, c.OrderID); response.Code != http.StatusOK {
		t.Fatalf("resume trusted Confirm: %d %s", response.Code, response.Body.String())
	}
	retried, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
	if e != nil || retried.OrderID != c.OrderID || retried.State != "paid" {
		t.Fatalf("resume %+v %v", retried, e)
	}
	f.assertEffects(t, c.OrderID, "paid", 1, 1, 1, 98, 0)
	captured := 0
	for _, event := range f.fake.Events() {
		if event.Status == payment.StatusCaptured {
			captured++
		}
	}
	if captured != 1 {
		t.Fatalf("capture effects=%d", captured)
	}
}
