//go:build pgx

// Responsibility: exercise approved quote -> immutable order -> Fake prepay in PG.
// Dependencies: public quotes/orders/ledger ports; pgxadapter owns the pool.
// Invariants: expendable fixtures only, runtime SQL uses app_runtime; this TEST
// adapter holds a transaction around deterministic in-memory Fake IO. Its mutex
// and cache are not durable checkout recovery and are never production wiring.
package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	lc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type quoteOrderRunner struct{ pgxadapter.Runner }

func (r quoteOrderRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
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
	prepay                                                                                                 *quoteOrderFakePrepayment
	app                                                                                                    *quotes.ConversionApplication
}

func quoteOrdersDB(t *testing.T, expiry time.Duration) *quoteOrderFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Fatal("NOT_RUN: mandatory expendable migrated PostgreSQL DATABASE_URL")
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
			{`INSERT INTO app.products(tenant_id,id,product_code) VALUES($1,$2,'quote-orders')`, []any{f.tenant, f.product}},
			{`INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key) VALUES($1,$2,$3,'QUOTE-ORDER-SKU','default')`, []any{f.tenant, f.sku, f.product}},
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
	f.prepay = &quoteOrderFakePrepayment{f: f, terminal: map[string]qc.Prepayment{}}
	f.app = quotes.NewConversionApplication(quotes.ConversionConfig{Runner: f.runner, Quotes: m, Orders: orders.NewQuoteCreator(), Checkout: f.prepay})
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

// quoteOrderFakePrepayment is deliberately TEST ONLY. Real checkout must persist
// provider claims/reconciliation; this local mutex/cache proves neither crashes
// nor network ambiguity. No production adapter is inferred from this harness.
type quoteOrderFakePrepayment struct {
	mu                                     sync.Mutex
	f                                      *quoteOrderFixture
	terminal                               map[string]qc.Prepayment
	createCalls, captureCalls, cancelCalls int
	beforeValidate                         func(context.Context, string) error
}

func (p *quoteOrderFakePrepayment) Prepay(ctx context.Context, actor qc.AuthorizedActor, c qc.Conversion) (qc.Prepayment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if out, ok := p.terminal[c.OrderID]; ok {
		return out, nil
	}
	out := qc.Prepayment{OrderID: c.OrderID, Amount: c.Amount, State: string(oc.PendingPayment)}
	var payable oc.Payable
	err := p.f.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		payable, e = orders.New().GetPayable(ctx, tx, actor.TenantID(), c.OrderID)
		return e
	})
	if err != nil {
		return out, err
	}
	req := payment.CreatePaymentIntent{TenantID: actor.TenantID(), OrderID: c.OrderID, Amount: payable.Amount, OrderVersion: payable.Version, IdempotencyKey: "quote-test:create:" + c.OrderID}
	p.createCalls++
	intent, err := p.f.fake.CreateIntent(ctx, req)
	if err != nil {
		return out, err
	}
	out.ProviderRef = intent.ProviderRef
	op := payment.PaymentOperation{CreatePaymentIntent: req, ProviderRef: intent.ProviderRef}
	op.IdempotencyKey = "quote-test:capture:" + c.OrderID
	if p.beforeValidate != nil {
		hook := p.beforeValidate
		p.beforeValidate = nil
		if e := hook(ctx, c.OrderID); e != nil {
			return out, e
		}
	}
	var invalidHold, definitive bool
	err = p.f.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		if e := orders.NewReservationValidator().ValidateReservations(ctx, tx, actor.TenantID(), c.OrderID); e != nil {
			invalidHold = errors.Is(e, oc.ErrConflict) || errors.Is(e, oc.ErrStockUnavailable)
			return e
		}
		p.captureCalls++
		captured, e := p.f.fake.Capture(ctx, op)
		if e != nil {
			var pe *payment.PaymentError
			definitive = errors.As(e, &pe) && pe.Code == payment.ErrorNonRetryable
			return e
		}
		if captured.Status != payment.StatusCaptured {
			return errors.New("test Fake capture was not captured")
		}
		_, e = orders.New().Transition(ctx, tx, actor.TenantID(), oc.TransitionInput{OrderID: c.OrderID, Target: oc.Paid, EventKey: "quote-test:paid:" + c.OrderID})
		if e != nil {
			return e
		}
		q, e := quotes.New().Get(ctx, tx, actor, c.QuoteID)
		if e != nil {
			return e
		}
		// Same account meanings as B2C: recognize sale/receivable, then settle cash.
		entries := []lc.Entry{}
		add := func(code string, side lc.Side, amount int64) {
			if amount > 0 {
				entries = append(entries, lc.Entry{AccountID: p.f.accounts[code], Side: side, Amount: money.Money{Currency: q.Currency, Amount: amount}})
			}
		}
		add("accounts_receivable", lc.Debit, q.Snapshot.Total)
		add("revenue", lc.Credit, q.Snapshot.Subtotal)
		add("tax_payable", lc.Credit, q.Snapshot.TaxAmount)
		add("shipping_revenue", lc.Credit, q.Snapshot.ShippingAmount)
		add("cash", lc.Debit, q.Snapshot.Total)
		add("accounts_receivable", lc.Credit, q.Snapshot.Total)
		_, e = ledger.New().Post(ctx, tx, lc.Transaction{TenantID: actor.TenantID(), Currency: q.Currency, SourceType: "order", SourceID: c.OrderID, Event: "quote-test:capture", Entries: entries})
		return e
	})
	if err != nil {
		if !invalidHold && !definitive {
			return out, err
		} // Ambiguous failure keeps stock held.
		op.IdempotencyKey = "quote-test:cancel:" + c.OrderID
		p.cancelCalls++
		if _, e := p.f.fake.Cancel(ctx, op); e != nil {
			return out, e
		}
		err = p.f.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
			_, e := orders.New().Transition(ctx, tx, actor.TenantID(), oc.TransitionInput{OrderID: c.OrderID, Target: oc.Cancelled, EventKey: "quote-test:cancelled:" + c.OrderID})
			return e
		})
		if err != nil {
			return out, err
		}
		out.State = string(oc.Cancelled)
	} else {
		out.State = string(oc.Paid)
	}
	p.terminal[c.OrderID] = out
	return out, nil
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
			if createdEvents != 1 || convertedEvents != 1 || cancelledEvents != wantCancel {
				return fmt.Errorf("outbox created=%d converted=%d cancelled=%d", createdEvents, convertedEvents, cancelledEvents)
			}
		}
		if journals > 0 {
			var debit, credit int64
			if e := tx.QueryRow(ctx, `SELECT coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint FROM app.ledger_entries WHERE tenant_id=$1`, f.tenant).Scan(&debit, &credit); e != nil {
				return e
			}
			if debit != 540 || credit != 540 {
				return fmt.Errorf("journal balance %d/%d", debit, credit)
			}
			var exact bool
			if e := tx.QueryRow(ctx, `WITH actual AS (SELECT a.code,e.side,sum(e.amount)::bigint AS amount FROM app.ledger_entries e JOIN app.ledger_accounts a ON a.tenant_id=e.tenant_id AND a.id=e.account_id WHERE e.tenant_id=$1 GROUP BY a.code,e.side), expected(code,side,amount) AS (VALUES('accounts_receivable','debit',270::bigint),('accounts_receivable','credit',270::bigint),('cash','debit',270::bigint),('revenue','credit',200::bigint),('tax_payable','credit',20::bigint),('shipping_revenue','credit',50::bigint)) SELECT NOT EXISTS((SELECT * FROM actual EXCEPT ALL SELECT * FROM expected) UNION ALL (SELECT * FROM expected EXCEPT ALL SELECT * FROM actual))`, f.tenant).Scan(&exact); e != nil {
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
	first, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
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
			c, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
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
		if id != c.OrderID || c.State != "paid" {
			t.Fatalf("concurrent result %+v", c)
		}
	}
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 1 {
		t.Fatalf("repeated effect %d/%d", f.prepay.createCalls, f.prepay.captureCalls)
	}
	f.assertEffects(t, id, "paid", 1, 1, 1, 98, 0)
}

func TestQuoteOrdersPGReplayRechecksMembership(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	key := uuid.NewString()
	c, err := f.app.Convert(f.buyerCtx, f.quote.ID, key)
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
			} else {
				f.prepay.beforeValidate = func(ctx context.Context, id string) error {
					return f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
						_, e := tx.Exec(ctx, `UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_id=$2`, f.tenant, id)
						return e
					})
				}
			}
			key := uuid.NewString()
			c, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
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
	c, e := f.app.Convert(f.buyerCtx, f.quote.ID, key)
	var pe *payment.PaymentError
	if !errors.As(e, &pe) || !pe.Retryable() {
		t.Fatalf("ambiguous result %+v %v", c, e)
	}
	if f.prepay.cancelCalls != 0 {
		t.Fatal("ambiguous failure canceled intent")
	}
	f.assertEffects(t, c.OrderID, "pending_payment", 1, 0, 0, 100, 2)
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
