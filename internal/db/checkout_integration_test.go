//go:build pgx

// Responsibility: exercise Checkout, Orders, Ledger and the Fake Payment Port
// end to end against expendable, migrated PostgreSQL, without skipped cases.
// Dependencies: public module contracts and the central pgxadapter. These
// orchestration cases use intent fixtures; checkout_cart_integration_test.go
// covers the real Cart reader. Prices/tax/shipping are SQL.
// Invariants: runtime-scoped transactions, durable replay, one terminal winner,
// no overselling and no provider IO while the calling transaction remains open.
package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type checkoutCartFixture struct {
	tenant string
	cart   checkoutcontract.Cart
}

func (c checkoutCartFixture) Read(_ context.Context, _ pgx.Tx, actor checkoutcontract.AuthorizedActor, id string) (checkoutcontract.Cart, error) {
	if actor.TenantID() != c.tenant || id != c.cart.ID {
		return checkoutcontract.Cart{}, checkoutcontract.ErrNotFound
	}
	return c.cart, nil
}

type checkoutDepthKey struct{}

// Each concurrent request gets its own marker, so another request's legitimate
// transaction cannot make this observation falsely report provider IO in a tx.
func checkoutRequestContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkoutDepthKey{}, &atomic.Int32{})
}

type checkoutPaymentProbe struct {
	fake       *paymentfake.Fake
	captures   atomic.Int32
	cancels    atomic.Int32
	violations atomic.Int32
	// before is installed before starting goroutines, and never mutated in flight.
	before func(context.Context, paymentfake.Operation) error
}

func (p *checkoutPaymentProbe) check(ctx context.Context, op paymentfake.Operation) error {
	if depth, ok := ctx.Value(checkoutDepthKey{}).(*atomic.Int32); ok && depth.Load() != 0 {
		p.violations.Add(1)
		return errors.New("provider called inside database transaction")
	}
	if p.before != nil {
		return p.before(ctx, op)
	}
	return nil
}
func (p *checkoutPaymentProbe) CreateIntent(ctx context.Context, in payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Create); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return p.fake.CreateIntent(ctx, in)
}
func (p *checkoutPaymentProbe) Capture(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Capture); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	p.captures.Add(1)
	return p.fake.Capture(ctx, in)
}
func (p *checkoutPaymentProbe) Cancel(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Cancel); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	p.cancels.Add(1)
	return p.fake.Cancel(ctx, in)
}
func (p *checkoutPaymentProbe) Refund(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := p.check(ctx, paymentfake.Refund); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return p.fake.Refund(ctx, in)
}

type checkoutPGFixture struct {
	ctx                  context.Context
	runner               pgxadapter.Runner
	tenant, sku, profile string
	actor                checkoutcontract.AuthorizedActor
	cart                 checkoutCartFixture
	provider             *checkoutPaymentProbe
	service              checkoutcontract.Service
	rollbackPaid         atomic.Bool
}

var errCheckoutInjectedCommit = errors.New("injected checkout finalize rollback")

func newCheckoutPGFixture(t *testing.T, stock, quantity int64) *checkoutPGFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "DATABASE_URL not set: Checkout PG gate requires an expendable migrated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	r, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePool)
	f := &checkoutPGFixture{ctx: ctx, runner: r, tenant: uuid.NewString(), sku: uuid.NewString(), profile: uuid.NewString(), provider: &checkoutPaymentProbe{fake: paymentfake.New()}}
	guest := sha256.Sum256([]byte(uuid.NewString()))
	f.actor, err = checkoutcontract.NewAuthorizedActor(f.tenant, "guest", hex.EncodeToString(guest[:]))
	if err != nil {
		t.Fatal(err)
	}
	f.cart = checkoutCartFixture{tenant: f.tenant, cart: checkoutcontract.Cart{ID: uuid.NewString(), Currency: "USD", Version: 1, Lines: []checkoutcontract.CartLine{{ID: uuid.NewString(), SKUID: f.sku, Quantity: quantity}}}}
	err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		product, priceList, taxZone, shippingZone := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		statements := []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO app.tenants(id,name,settlement_currency) VALUES($1,'checkout-pgx-gate','USD')", []any{f.tenant}},
			{"INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES($1,$2,'CHECKOUT-P','ready')", []any{f.tenant, product}},
			{"INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES($1,$2,$3,'CHECKOUT-SKU','default','ready')", []any{f.tenant, f.sku, product}},
			{"INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at) VALUES($1,$2,'USD','active',transaction_timestamp()-interval '1 day')", []any{f.tenant, priceList}},
			{"INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES($1,$2,$3,1,100)", []any{f.tenant, priceList, f.sku}},
			{"INSERT INTO app.tax_settings(tenant_id,prices_include_tax) VALUES($1,false)", []any{f.tenant}},
			{"INSERT INTO app.tax_zones(tenant_id,id,name) VALUES($1,$2,'United States')", []any{f.tenant, taxZone}},
			{"INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')", []any{f.tenant, taxZone}},
			{"INSERT INTO app.tax_categories(tenant_id,code,name) VALUES($1,'standard','Standard')", []any{f.tenant}},
			{"INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES($1,$2,'standard',1000,transaction_timestamp()-interval '1 day')", []any{f.tenant, taxZone}},
			{"INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'US shipping')", []any{f.tenant, shippingZone}},
			{"INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')", []any{f.tenant, shippingZone}},
			{"INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES($1,$2,'Standard','USD')", []any{f.tenant, f.profile}},
			{"INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',50)", []any{f.tenant, f.profile, shippingZone}},
			{"SELECT set_config('app.tenant_id',$1,true)", []any{f.tenant}},
			{"SELECT app.inventory_adjust($1,$2,'checkout test receipt',$3,'inbound')", []any{f.sku, stock, uuid.NewString()}},
		}
		for _, s := range statements {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		for code, kind := range map[string]string{"cash": "asset", "accounts_receivable": "asset", "revenue": "revenue", "tax_payable": "liability", "shipping_revenue": "revenue"} {
			if _, err := tx.Exec(ctx, "INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES($1,$2,'USD',$3)", f.tenant, code, kind); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}})
	f.service = checkout.New(checkout.Config{Run: f.run, Cart: f.cart, Rules: rules, Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: f.provider})
	t.Cleanup(func() {
		if n := f.provider.violations.Load(); n != 0 {
			t.Errorf("provider IO observed in %d transactions", n)
		}
	})
	return f
}

func (f *checkoutPGFixture) run(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if depth, ok := ctx.Value(checkoutDepthKey{}).(*atomic.Int32); ok {
		depth.Add(1)
		defer depth.Add(-1)
	}
	return f.runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if f.rollbackPaid.Load() {
			var posted bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM app.checkout_sessions WHERE tenant_id=$1 AND phase='paid')", tenant).Scan(&posted); err != nil {
				return err
			}
			if posted && f.rollbackPaid.CompareAndSwap(true, false) {
				return errCheckoutInjectedCommit
			}
		}
		return nil
	})
}

func (f *checkoutPGFixture) input() checkoutcontract.StartInput {
	return checkoutcontract.StartInput{AcceptedTotal: checkoutcontract.AcceptedTotal{AmountMinor: 110*f.cart.cart.Lines[0].Quantity + 50, Currency: "USD"}, CartID: f.cart.cart.ID, BuyerContact: orderscontract.Contact{Email: "buyer@example.test", Name: "Fixture buyer"}, ShippingAddress: checkoutcontract.ShippingAddress{Country: "US", Address1: "100 Test Avenue", City: "Test City", PostalCode: "10000"}, IdempotencyKey: "start:" + uuid.NewString()}
}
func (f *checkoutPGFixture) start(t *testing.T) checkoutcontract.Result {
	t.Helper()
	r, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, f.input())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *checkoutPGFixture) event(r checkoutcontract.Result, status payment.PaymentStatus) payment.VerifiedEvent {
	return payment.VerifiedEvent{EventID: "event:" + uuid.NewString(), TenantID: f.tenant, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: r.Payment.ProviderRef, Status: status, Amount: money.Money{Amount: r.AmountMinor, Currency: r.Currency}}
}

type checkoutWitness struct {
	state, phase                                                                                                string
	onHand, reserved, journals, entries, debits, credits, receivable, cash, paidEvents, cancelEvents, movements int64
}

func (f *checkoutPGFixture) witness(t *testing.T, order string) checkoutWitness {
	t.Helper()
	var w checkoutWitness
	err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		queries := []struct {
			sql  string
			dst  []any
			args []any
		}{
			{"SELECT state FROM app.orders WHERE tenant_id=$1 AND id=$2", []any{&w.state}, []any{f.tenant, order}},
			{"SELECT phase FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2", []any{&w.phase}, []any{f.tenant, order}},
			{"SELECT on_hand,reserved FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", []any{&w.onHand, &w.reserved}, []any{f.tenant, f.sku}},
			{"SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_id=$2::text", []any{&w.journals}, []any{f.tenant, order}},
			{"SELECT count(*),coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint FROM app.ledger_entries e JOIN app.ledger_transactions x ON (x.tenant_id,x.id)=(e.tenant_id,e.transaction_id) WHERE x.tenant_id=$1 AND x.source_id=$2::text", []any{&w.entries, &w.debits, &w.credits}, []any{f.tenant, order}},
			{"SELECT coalesce(sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END),0)::bigint FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=$1 AND a.code='accounts_receivable'", []any{&w.receivable}, []any{f.tenant}},
			{"SELECT coalesce(sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END),0)::bigint FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=$1 AND a.code='cash'", []any{&w.cash}, []any{f.tenant}},
			{"SELECT count(*) FILTER(WHERE event_type='order.paid'),count(*) FILTER(WHERE event_type='order.cancelled') FROM app.outbox_events WHERE tenant_id=$1 AND payload->>'order_id'=$2::text", []any{&w.paidEvents, &w.cancelEvents}, []any{f.tenant, order}},
			{"SELECT count(*) FROM app.inventory_movements WHERE tenant_id=$1 AND owner_id=$2", []any{&w.movements}, []any{f.tenant, order}},
		}
		for _, q := range queries {
			if err := tx.QueryRow(ctx, q.sql, q.args...).Scan(q.dst...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCheckoutPGGuestPaidBalancedReplay(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	in := f.input()
	ctx := checkoutRequestContext(f.ctx)
	start, err := f.service.Start(ctx, f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	if start.AmountMinor != 270 || start.Currency != "USD" || start.State != "ready" {
		t.Fatalf("calculated payable %#v", start)
	}
	replayed, err := f.service.Start(ctx, f.actor, in)
	if err != nil || replayed != start {
		t.Fatalf("Start replay %#v %v", replayed, err)
	}
	event := f.event(start, payment.StatusAuthorized)
	paid, err := f.service.Confirm(ctx, event)
	if err != nil || paid.State != "paid" {
		t.Fatalf("Confirm %#v %v", paid, err)
	}
	w := f.witness(t, start.OrderID)
	if w.state != "paid" || w.phase != "paid" || w.onHand != 8 || w.reserved != 0 || w.journals != 1 || w.entries != 6 || w.debits != 540 || w.credits != 540 || w.receivable != 0 || w.cash != 270 || w.paidEvents != 1 || w.cancelEvents != 0 {
		t.Fatalf("paid witness %+v", w)
	}
	for range 3 {
		r, e := f.service.Confirm(ctx, event)
		if e != nil || r != paid {
			t.Fatalf("webhook replay %#v %v", r, e)
		}
	}
	if got := f.witness(t, start.OrderID); got != w {
		t.Fatalf("duplicate webhook changed effects: before %+v after %+v", w, got)
	}
	if f.provider.captures.Load() != 1 {
		t.Fatalf("duplicate event called Capture %d times", f.provider.captures.Load())
	}
	// Replaying Start after payment returns the original intent/payable snapshot.
	r, e := f.service.Start(ctx, f.actor, in)
	if e != nil || r != start {
		t.Fatalf("Start after terminal %#v %v", r, e)
	}
	changed := event
	changed.Status = payment.StatusCaptured
	if _, e = f.service.Confirm(ctx, changed); !errors.Is(e, checkoutcontract.ErrKeyReused) {
		t.Fatalf("changed event content: %v", e)
	}
	changedInput := in
	changedInput.ShippingAddress.Address1 = "Changed address"
	if _, e = f.service.Start(ctx, f.actor, changedInput); !errors.Is(e, checkoutcontract.ErrKeyReused) {
		t.Fatalf("changed Start content: %v", e)
	}
}

func TestCheckoutPGFailedPaymentReleasesWithoutLedger(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	start := f.start(t)
	failed, err := f.service.Confirm(checkoutRequestContext(f.ctx), f.event(start, payment.StatusFailed))
	if err != nil || failed.State != "failed" {
		t.Fatalf("failed payment %#v %v", failed, err)
	}
	w := f.witness(t, start.OrderID)
	if w.state != "cancelled" || w.phase != "failed" || w.onHand != 10 || w.reserved != 0 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 || w.cancelEvents != 1 {
		t.Fatalf("failure effects %+v", w)
	}
	if f.provider.captures.Load() != 0 || f.provider.cancels.Load() != 1 {
		t.Fatal("failure called wrong provider operation")
	}
}

func TestCheckoutPGProviderFailuresReleaseWithoutLedger(t *testing.T) {
	for _, operation := range []paymentfake.Operation{paymentfake.Create, paymentfake.Capture} {
		t.Run(string(operation), func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			in := f.input()
			if operation == paymentfake.Create {
				f.provider.fake.FailNext(operation, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "injected test denial"})
			}
			start, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
			if err != nil {
				t.Fatal(err)
			}
			terminal := start
			var event payment.VerifiedEvent
			if operation == paymentfake.Capture {
				f.provider.fake.FailNext(operation, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "injected test denial"})
				event = f.event(start, payment.StatusAuthorized)
				terminal, err = f.service.Confirm(checkoutRequestContext(f.ctx), event)
				if err != nil {
					t.Fatal(err)
				}
			}
			if terminal.State != "failed" {
				t.Fatalf("provider failure result %#v", terminal)
			}
			w := f.witness(t, start.OrderID)
			if w.state != "cancelled" || w.phase != "failed" || w.onHand != 10 || w.reserved != 0 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 || w.cancelEvents != 1 {
				t.Fatalf("provider failure effects %+v", w)
			}
			var replay checkoutcontract.Result
			if operation == paymentfake.Create {
				replay, err = f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
			} else {
				replay, err = f.service.Confirm(checkoutRequestContext(f.ctx), event)
			}
			if err != nil || replay != terminal {
				t.Fatalf("failed command replay %#v %v", replay, err)
			}
			if got := f.witness(t, start.OrderID); got != w {
				t.Fatalf("failed replay changed effects %+v -> %+v", w, got)
			}
		})
	}
}

func TestCheckoutPGUnreadySKUHasNoEffects(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.product_variants SET lifecycle_status='draft' WHERE tenant_id=$1 AND id=$2", f.tenant, f.sku)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, f.input()); err == nil {
		t.Fatal("draft SKU checkout succeeded")
	}
	if len(f.provider.fake.Events()) != 0 {
		t.Fatal("draft SKU reached provider")
	}
	var sessions, ordersCount, reservations int64
	if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1),(SELECT count(*) FROM app.orders WHERE tenant_id=$1),(SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1)", f.tenant).Scan(&sessions, &ordersCount, &reservations)
	}); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || ordersCount != 0 || reservations != 0 {
		t.Fatalf("draft SKU left effects sessions=%d orders=%d reservations=%d", sessions, ordersCount, reservations)
	}
}

func TestCheckoutPGCaptureCommitFailureRecovers(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	start := f.start(t)
	event := f.event(start, payment.StatusAuthorized)
	f.rollbackPaid.Store(true)
	if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), event); !errors.Is(err, errCheckoutInjectedCommit) {
		t.Fatalf("fault not reached: %v", err)
	}
	w := f.witness(t, start.OrderID)
	if w.state != "pending_payment" || w.phase != "confirming" || w.onHand != 10 || w.reserved != 2 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 {
		t.Fatalf("finalize rollback %+v", w)
	}
	if f.provider.captures.Load() != 1 {
		t.Fatal("capture did not succeed before injected rollback")
	}
	paid, err := f.service.Confirm(checkoutRequestContext(f.ctx), event)
	if err != nil || paid.State != "paid" {
		t.Fatalf("recovery %#v %v", paid, err)
	}
	w = f.witness(t, start.OrderID)
	if w.journals != 1 || w.cash != 270 || w.onHand != 8 || w.reserved != 0 || w.paidEvents != 1 {
		t.Fatalf("recovery effects %+v", w)
	}
	if f.provider.captures.Load() != 2 {
		t.Fatal("recovery did not replay the stable provider key")
	}
}

func TestCheckoutPGEventAndActorCannotCrossBinding(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	start := f.start(t)
	event := f.event(start, payment.StatusAuthorized)
	for _, change := range []func(*payment.VerifiedEvent){func(e *payment.VerifiedEvent) { e.Amount.Amount++ }, func(e *payment.VerifiedEvent) { e.Amount.Currency = "EUR" }, func(e *payment.VerifiedEvent) { e.OrderVersion++ }, func(e *payment.VerifiedEvent) { e.ProviderRef = "unrelated-provider" }} {
		bad := event
		change(&bad)
		if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), bad); !errors.Is(err, checkoutcontract.ErrConflict) {
			t.Fatalf("payable binding accepted: %v", err)
		}
	}
	foreignHash := sha256.Sum256([]byte(uuid.NewString()))
	other, err := checkoutcontract.NewAuthorizedActor(f.tenant, "guest", hex.EncodeToString(foreignHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Cancel(checkoutRequestContext(f.ctx), other, checkoutcontract.CancelInput{CheckoutID: start.CheckoutID, IdempotencyKey: "cancel-other"}); !errors.Is(err, checkoutcontract.ErrNotFound) {
		t.Fatalf("foreign actor cancelled: %v", err)
	}
	if f.provider.captures.Load() != 0 || f.provider.cancels.Load() != 0 {
		t.Fatal("unbound input reached provider")
	}
}

// Blocking at the provider boundary proves the durable winner is committed
// BEFORE external IO; the losing command finishes while that IO is still parked.
func TestCheckoutPGConfirmCancelOneWinner(t *testing.T) {
	for _, winner := range []paymentfake.Operation{paymentfake.Capture, paymentfake.Cancel} {
		t.Run(string(winner), func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			start := f.start(t)
			event := f.event(start, payment.StatusAuthorized)
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			f.provider.before = func(ctx context.Context, op paymentfake.Operation) error {
				if op == winner {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			type outcome struct {
				result checkoutcontract.Result
				err    error
			}
			finished := make(chan outcome, 1)
			cancel := checkoutcontract.CancelInput{CheckoutID: start.CheckoutID, IdempotencyKey: "cancel:" + uuid.NewString()}
			go func() {
				var r checkoutcontract.Result
				var e error
				if winner == paymentfake.Capture {
					r, e = f.service.Confirm(checkoutRequestContext(f.ctx), event)
				} else {
					r, e = f.service.Cancel(checkoutRequestContext(f.ctx), f.actor, cancel)
				}
				finished <- outcome{r, e}
			}()
			select {
			case <-entered:
			case <-f.ctx.Done():
				t.Fatal("winner never reached provider")
			}
			var loser error
			if winner == paymentfake.Capture {
				_, loser = f.service.Cancel(checkoutRequestContext(f.ctx), f.actor, cancel)
			} else {
				_, loser = f.service.Confirm(checkoutRequestContext(f.ctx), event)
			}
			if !errors.Is(loser, checkoutcontract.ErrConflict) {
				t.Fatalf("loser was not fenced: %v", loser)
			}
			close(release)
			done := <-finished
			if done.err != nil {
				t.Fatal(done.err)
			}
			w := f.witness(t, start.OrderID)
			if winner == paymentfake.Capture {
				if w.state != "paid" || w.journals != 1 || w.paidEvents != 1 || f.provider.cancels.Load() != 0 {
					t.Fatalf("confirm winner %+v", w)
				}
			} else {
				if w.state != "cancelled" || w.journals != 0 || w.cancelEvents != 1 || w.onHand != 10 || f.provider.captures.Load() != 0 {
					t.Fatalf("cancel winner %+v", w)
				}
			}
		})
	}
}

func TestCheckoutPGLastItemCannotOversell(t *testing.T) {
	f := newCheckoutPGFixture(t, 1, 1)
	ready := make(chan struct{})
	type outcome struct {
		result checkoutcontract.Result
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		in := f.input()
		go func() {
			<-ready
			r, e := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
			results <- outcome{r, e}
		}()
	}
	close(ready)
	wins, losses := 0, 0
	var winning checkoutcontract.Result
	for range 2 {
		out := <-results
		if out.err == nil {
			wins++
			winning = out.result
		} else if errors.Is(out.err, orderscontract.ErrStockUnavailable) {
			losses++
		} else {
			t.Fatalf("unexpected contender error %v", out.err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("last item winners=%d stock failures=%d", wins, losses)
	}
	w := f.witness(t, winning.OrderID)
	if w.onHand != 1 || w.reserved != 1 || w.journals != 0 {
		t.Fatalf("last item witness %+v", w)
	}
	var sessions, ordersCount int64
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1),(SELECT count(*) FROM app.orders WHERE tenant_id=$1)", f.tenant).Scan(&sessions, &ordersCount)
	}); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || ordersCount != 1 {
		t.Fatalf("losing checkout left sessions=%d orders=%d", sessions, ordersCount)
	}
}

// A reservation that lapses between Start and Confirm must never reach Capture:
// the claim locks the order's holds, refuses capture, voids the intent and
// cancels the order. Both an unswept deadline and a swept (expired) hold count.
func TestCheckoutPGExpiredReservationIsNotCaptured(t *testing.T) {
	for _, swept := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline-passed", true: "expired-by-sweep"}[swept], func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			start := f.start(t)
			err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				// Only the trusted fixture administrator advances this order's deadline.
				_, err := tx.Exec(ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_id=$2", f.tenant, start.OrderID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if swept {
				queue, err := pgxadapter.OpenQueuePool(f.ctx, os.Getenv("DATABASE_URL"), "app_queue_worker")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(queue.Close)
				err = pgxadapter.InQueueTenant(f.ctx, queue, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					var n int
					if err := tx.QueryRow(ctx, "SELECT app.inventory_expire_due(100)").Scan(&n); err != nil || n != 1 {
						return errors.Join(err, errors.New("expiry sweep did not expire the hold"))
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := checkoutRequestContext(f.ctx)
			event := f.event(start, payment.StatusAuthorized)
			got, err := f.service.Confirm(ctx, event)
			if err != nil || got.State != "failed" || got.Payment.Status != payment.StatusCanceled {
				t.Fatalf("expired reservation Confirm %#v %v", got, err)
			}
			if c, k := f.provider.captures.Load(), f.provider.cancels.Load(); c != 0 || k != 1 {
				t.Fatalf("provider captures=%d cancels=%d, want 0/1", c, k)
			}
			w := f.witness(t, start.OrderID)
			if w.state != "cancelled" || w.phase != "failed" || w.onHand != 10 || w.reserved != 0 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 || w.cancelEvents != 1 {
				t.Fatalf("expired reservation effects %+v", w)
			}
			replay, err := f.service.Confirm(ctx, event)
			if err != nil || replay != got || f.provider.captures.Load() != 0 {
				t.Fatalf("replay %#v %v captures=%d", replay, err, f.provider.captures.Load())
			}
		})
	}
}

// ADR-C13a: only the selected profile's zones compete. Another profile's
// narrower (US-CA) or equally specific (US) zone must not shadow profile A's
// national rate, while A's own narrower zone still wins.
func TestCheckoutPGShippingZonesScopedToProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownCA    bool
		otherUS  bool
		expected int64
	}{{"other-narrower", false, false, 270}, {"other-narrower-and-equal", false, true, 270}, {"own-narrower-wins", true, true, 290}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			other, otherCA, otherUS, ownCA := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			stmts := [][]any{
				{"INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES($1,$2,'Other','USD')", f.tenant, other},
				{"INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'Other California')", f.tenant, otherCA},
				{"INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code,subdivision_code) VALUES($1,$2,'US','US-CA')", f.tenant, otherCA},
				{"INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',999)", f.tenant, other, otherCA},
			}
			if tc.otherUS {
				stmts = append(stmts,
					[]any{"INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'Other US')", f.tenant, otherUS},
					[]any{"INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code) VALUES($1,$2,'US')", f.tenant, otherUS},
					[]any{"INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',888)", f.tenant, other, otherUS})
			}
			if tc.ownCA {
				stmts = append(stmts,
					[]any{"INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES($1,$2,'Own California')", f.tenant, ownCA},
					[]any{"INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code,subdivision_code) VALUES($1,$2,'US','US-CA')", f.tenant, ownCA},
					[]any{"INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES($1,$2,$3,'fixed','USD',70)", f.tenant, f.profile, ownCA})
			}
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				for _, s := range stmts {
					if _, err := tx.Exec(ctx, s[0].(string), s[1:]...); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			in := f.input()
			in.ShippingAddress.Subdivision = "US-CA"
			in.AcceptedTotal.AmountMinor = tc.expected
			got, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
			if err != nil || got.AmountMinor != tc.expected {
				t.Fatalf("profile-scoped shipping Start %#v %v, want amount %d", got, err, tc.expected)
			}
		})
	}
}
