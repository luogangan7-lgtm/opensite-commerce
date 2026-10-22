//go:build pgx

// Responsibility: require production checkout adoption of an already sealed
// customer order, then exercise trusted Confirm commands and durable effects.
// Dependencies: existing checkout/order PG fixtures and neutral Payment probe.
// Callers: CI pgx gate after copying this file to internal/db. Invariants: no
// Cart, Rules, order recreation, provider IO in TX, or test-owned payment state.
package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

// This runtime assertion compiles before the production adoption API exists.
// Its first RED must be this missing capability, rather than a missing symbol.
type checkoutExistingOrderAdopter interface {
	AdoptOrder(context.Context, checkoutcontract.AuthorizedActor, string, string) (checkoutcontract.Result, error)
}

type checkoutAdoptionOrdersProbe struct {
	orderscontract.Service
	creates atomic.Int32
}

func (p *checkoutAdoptionOrdersProbe) Create(context.Context, pgx.Tx, string, orderscontract.CreateInput) (orderscontract.Order, error) {
	p.creates.Add(1)
	return orderscontract.Order{}, errors.New("adoption attempted to recreate an existing order")
}

type checkoutAdoptionPaymentProbe struct {
	*checkoutPaymentProbe
	creates atomic.Int32
}

func (p *checkoutAdoptionPaymentProbe) CreateIntent(ctx context.Context, in payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	p.creates.Add(1)
	return p.checkoutPaymentProbe.CreateIntent(ctx, in)
}

type checkoutAdoptionPG struct {
	f             *checkoutPGFixture
	adopter       checkoutExistingOrderAdopter
	order         orderscontract.Order
	wrongCustomer string
	orders        *checkoutAdoptionOrdersProbe
	payment       *checkoutAdoptionPaymentProbe
}

func newCheckoutAdoptionPG(t *testing.T) checkoutAdoptionPG {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		pgxtest.Skip(t, "NOT_RUN: checkout adoption requires an expendable migrated PostgreSQL database")
	}
	f := newCheckoutPGFixture(t, 10, 2)
	customer, wrongCustomer := uuid.NewString(), uuid.NewString()
	if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,'adoption-owner@example.test'),($1,$3,'adoption-other@example.test')", f.tenant, customer, wrongCustomer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := checkoutcontract.NewAuthorizedActor(f.tenant, "customer", customer)
	if err != nil {
		t.Fatal(err)
	}
	f.actor = actor
	// Reuse the independent Orders fixture's frozen 200 net + 20 tax + 50
	// shipping input. Adoption receives no calculators, Cart or live Rules.
	in := (orderFixture{tenant: f.tenant, sku: f.sku}).input(t)
	in.Buyer = orderscontract.Buyer{CustomerID: customer}
	in.Lines[0].SKU = "CHECKOUT-SKU"
	var created orderscontract.Order
	if err := f.run(checkoutRequestContext(f.ctx), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = orders.New().Create(ctx, tx, f.tenant, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if created.State != orderscontract.PendingPayment || created.Total != (money.Money{Amount: 270, Currency: "USD"}) || created.Version != 1 {
		t.Fatalf("sealed seed order: %#v", created)
	}
	op := &checkoutAdoptionOrdersProbe{Service: orders.New()}
	pp := &checkoutAdoptionPaymentProbe{checkoutPaymentProbe: f.provider}
	f.service = checkout.New(checkout.Config{Run: f.run, Orders: op, Ledger: ledger.New(), Payment: pp})
	adopter, ok := any(f.service).(checkoutExistingOrderAdopter)
	if !ok {
		t.Fatal("FIX57_ADOPTION_MISSING: production checkout.New cannot adopt an existing sealed customer order")
	}
	return checkoutAdoptionPG{f: f, adopter: adopter, order: created, wrongCustomer: wrongCustomer, orders: op, payment: pp}
}

func (a checkoutAdoptionPG) adopt(t *testing.T, key string) checkoutcontract.Result {
	t.Helper()
	r, err := a.adopter.AdoptOrder(checkoutRequestContext(a.f.ctx), a.f.actor, a.order.ID, key)
	if err != nil || r.OrderID != a.order.ID || r.AmountMinor != 270 || r.Currency != "USD" || r.OrderVersion != 1 || r.State != "ready" || r.Payment.ProviderRef == "" {
		t.Fatalf("adopt original frozen order: %#v %v", r, err)
	}
	return r
}

func (a checkoutAdoptionPG) assertSnapshot(t *testing.T, r checkoutcontract.Result) {
	t.Helper()
	var customer, orderID, currency, source string
	var amount, net, tax, shipping, version, orderCount, sessions int64
	var noCartID, noCartVersion bool
	if err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.actor_id,s.order_id::text,s.currency,s.source_kind,s.amount_minor,s.items_net_minor,s.tax_minor,s.shipping_minor,s.payable_version,
		 s.cart_id IS NULL,s.cart_version IS NULL,
		 (SELECT count(*) FROM app.orders WHERE tenant_id=$1),
		 (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1)
		 FROM app.checkout_sessions s WHERE s.tenant_id=$1 AND s.id=$2`, a.f.tenant, r.CheckoutID).Scan(&customer, &orderID, &currency, &source, &amount, &net, &tax, &shipping, &version, &noCartID, &noCartVersion, &orderCount, &sessions)
	}); err != nil {
		t.Fatal(err)
	}
	if customer != a.f.actor.ID() || orderID != a.order.ID || currency != "USD" || source != "existing_order" || amount != 270 || net != 200 || tax != 20 || shipping != 50 || version != 1 || !noCartID || !noCartVersion || orderCount != 1 || sessions != 1 || a.orders.creates.Load() != 0 {
		t.Fatalf("adoption snapshot owner=%s order=%s %s amount=%d net=%d tax=%d shipping=%d v=%d nullCart=%v/%v orders=%d sessions=%d recreates=%d", customer, orderID, currency, amount, net, tax, shipping, version, noCartID, noCartVersion, orderCount, sessions, a.orders.creates.Load())
	}
}

// A committed capture followed by a rolled-back finalize is uncertain money.
// Expiry before recovery must preserve the durable claim and never void it.
func TestCheckoutPGAdoptionUncertainCaptureExpiredHoldStaysPending(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	start := a.adopt(t, "uncertain:"+uuid.NewString())
	event := a.commandEvent(t, start)
	a.f.rollbackPaid.Store(true)
	if _, err := a.f.service.Confirm(checkoutRequestContext(a.f.ctx), event); !errors.Is(err, errCheckoutInjectedCommit) {
		t.Fatalf("capture/finalize fault not reached: %v", err)
	}
	before := a.f.witness(t, start.OrderID)
	if before.phase != "confirming" || before.state != "pending_payment" || before.journals != 0 || a.f.provider.captures.Load() != 1 {
		t.Fatalf("uncertain capture witness: %+v captures=%d", before, a.f.provider.captures.Load())
	}
	if err := a.f.runner.InTenant(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_type='order' AND owner_id=$2", a.f.tenant, start.OrderID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := a.f.service.Confirm(checkoutRequestContext(a.f.ctx), event); !errors.Is(err, checkoutcontract.ErrRetry) {
			t.Fatalf("expired uncertain recovery must stay retryable: %v", err)
		}
	}
	if after := a.f.witness(t, start.OrderID); after != before || a.f.provider.captures.Load() != 1 || a.f.provider.cancels.Load() != 0 || a.f.provider.violations.Load() != 0 {
		t.Fatalf("uncertain money changed: %+v -> %+v captures=%d cancels=%d txIO=%d", before, after, a.f.provider.captures.Load(), a.f.provider.cancels.Load(), a.f.provider.violations.Load())
	}
	var incomplete bool
	if err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.final_result IS NULL AND a.result IS NULL AND s.claim_kind='payment' AND s.claim_key=$3
		 FROM app.checkout_sessions s JOIN app.checkout_attempts a ON a.tenant_id=s.tenant_id AND a.checkout_id=s.id
		 WHERE s.tenant_id=$1 AND s.id=$2 AND a.event_key=$3`, a.f.tenant, start.CheckoutID, event.EventID).Scan(&incomplete)
	}); err != nil || !incomplete {
		t.Fatalf("recovery claim/receipt lost: pending=%v %v", incomplete, err)
	}
}

func TestCheckoutPGAdoptionSourceAdmissionAndImmutableSnapshot(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	// The positive runtime SQL witness makes missing row-lock privileges fail
	// directly. Roll back the admitted receipt so later probes cannot hit unique.
	rollbackAdmission := errors.New("rollback successful runtime admission witness")
	err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var source string
		var noCartID, noCartVersion bool
		err := tx.QueryRow(ctx, `INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,source_kind,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
		 VALUES($1,$2,'customer',$3,$4,sha256('admission-positive'::bytea),'existing_order',$5,'{}','USD',270,200,20,50,1)
		 RETURNING source_kind,cart_id IS NULL,cart_version IS NULL`, a.f.tenant, uuid.NewString(), a.f.actor.ID(), "positive:"+uuid.NewString(), a.order.ID).Scan(&source, &noCartID, &noCartVersion)
		if err != nil {
			return err
		}
		if source != "existing_order" || !noCartID || !noCartVersion {
			return errors.New("runtime admission changed source/cart snapshot")
		}
		return rollbackAdmission
	})
	if !errors.Is(err, rollbackAdmission) {
		t.Fatalf("valid direct runtime source admission failed: %v", err)
	}
	// Probe real admission before a session exists, so neither uniqueness nor
	// service validation can mask a missing DB owner/source/component guard.
	for _, tc := range []struct {
		name, source, owner                string
		cartID                             any
		cartVersion                        any
		net, tax, shipping, total, version int64
	}{
		{"cart-without-cart", "cart", a.f.actor.ID(), nil, nil, 200, 20, 50, 270, 1},
		{"existing-with-cart-id", "existing_order", a.f.actor.ID(), uuid.NewString(), nil, 200, 20, 50, 270, 1},
		{"existing-with-cart-version", "existing_order", a.f.actor.ID(), nil, int64(1), 200, 20, 50, 270, 1},
		{"unknown-source", "unknown", a.f.actor.ID(), nil, nil, 200, 20, 50, 270, 1},
		{"foreign-owner", "existing_order", a.wrongCustomer, nil, nil, 200, 20, 50, 270, 1},
		{"net-tax-balanced-mismatch", "existing_order", a.f.actor.ID(), nil, nil, 201, 19, 50, 270, 1},
		{"shipping-net-balanced-mismatch", "existing_order", a.f.actor.ID(), nil, nil, 199, 20, 51, 270, 1},
		{"total-balanced-mismatch", "existing_order", a.f.actor.ID(), nil, nil, 201, 20, 50, 271, 1},
		{"payable-version-mismatch", "existing_order", a.f.actor.ID(), nil, nil, 200, 20, 50, 270, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,source_kind,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
				 VALUES($1,$2,'customer',$3,$4,sha256('admission-probe'::bytea),$5,$6,$7,$8,'{}','USD',$9,$10,$11,$12,$13)`, a.f.tenant, uuid.NewString(), tc.owner, "probe:"+uuid.NewString(), tc.source, tc.cartID, tc.cartVersion, a.order.ID, tc.total, tc.net, tc.tax, tc.shipping, tc.version)
				return err
			})
			var pe *pgxadapter.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" {
				t.Fatalf("DB must reject source/snapshot with check_violation: %v", err)
			}
		})
	}
	start := a.adopt(t, "immutable:"+uuid.NewString())
	// Runtime has no source update grant; administrator has the grant, but must
	// still fail the immutable guard even when the new cart source is well shaped.
	err = a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.checkout_sessions SET source_kind='cart',cart_id=$3,cart_version=1 WHERE tenant_id=$1 AND id=$2", a.f.tenant, start.CheckoutID, uuid.NewString())
		return err
	})
	var pe *pgxadapter.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatalf("runtime source update granted: %v", err)
	}
	err = a.f.runner.InTenant(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.checkout_sessions SET source_kind='cart',cart_id=$3,cart_version=1 WHERE tenant_id=$1 AND id=$2", a.f.tenant, start.CheckoutID, uuid.NewString())
		return err
	})
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("administrator rewrote source identity: %v", err)
	}
	a.assertSnapshot(t, start)
}

// These command tests use Fake's actual authorized intent. Public callbacks
// admit completed account-mapped facts, never requests to Capture. Their HTTP
// signature/admission path is covered by quoteorders_production_pgx_test.go.
func (a checkoutAdoptionPG) commandEvent(t *testing.T, r checkoutcontract.Result) payment.VerifiedEvent {
	t.Helper()
	for _, e := range a.f.provider.fake.Events() {
		if e.OrderID != r.OrderID || e.Status != payment.StatusAuthorized {
			continue
		}
		return payment.VerifiedEvent(e)
	}
	t.Fatal("provider did not emit an authorized event for the adopted order")
	return payment.VerifiedEvent{}
}

func (a checkoutAdoptionPG) confirm(t *testing.T, event payment.VerifiedEvent, state string) checkoutcontract.Result {
	t.Helper()
	result, err := a.f.service.Confirm(checkoutRequestContext(a.f.ctx), event)
	if err != nil || result.State != state {
		t.Fatalf("trusted Confirm expected %s: %#v %v", state, result, err)
	}
	return result
}

func TestCheckoutPGAdoptExistingCustomerOrderPaidBalancedReplay(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	key := "adopt:" + uuid.NewString()
	start := a.adopt(t, key)
	a.assertSnapshot(t, start)
	event := a.commandEvent(t, start)
	result := a.confirm(t, event, "paid")
	w := a.f.witness(t, a.order.ID)
	if w.state != "paid" || w.phase != "paid" || w.onHand != 8 || w.reserved != 0 || w.journals != 1 || w.entries != 6 || w.debits != 540 || w.credits != 540 || w.receivable != 0 || w.cash != 270 || w.paidEvents != 1 || w.cancelEvents != 0 {
		t.Fatalf("adopted paid effects: %+v", w)
	}
	for range 3 {
		if got := a.confirm(t, event, "paid"); got != result {
			t.Fatalf("trusted event replay changed response: %#v -> %#v", result, got)
		}
		if replay := a.adopt(t, key); replay != start {
			t.Fatalf("adoption must replay original payable/intent: %#v -> %#v", start, replay)
		}
	}
	a.assertSnapshot(t, start)
	if after := a.f.witness(t, a.order.ID); after != w || a.f.provider.captures.Load() != 1 || a.payment.creates.Load() != 1 {
		t.Fatalf("adoption replay changed effects: %+v -> %+v creates=%d captures=%d", w, after, a.payment.creates.Load(), a.f.provider.captures.Load())
	}
}

func TestCheckoutPGAdoptExistingOrderRejectsOtherCustomerAndGuest(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	wrong, err := checkoutcontract.NewAuthorizedActor(a.f.tenant, "customer", a.wrongCustomer)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("adoption-guest-test"))
	guest, err := checkoutcontract.NewAuthorizedActor(a.f.tenant, "guest", hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		actor checkoutcontract.AuthorizedActor
		want  error
	}{
		{"other-customer", wrong, checkoutcontract.ErrNotFound},
		{"guest", guest, checkoutcontract.ErrForbidden},
		{"absent-authority", checkoutcontract.AuthorizedActor{}, checkoutcontract.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.adopter.AdoptOrder(checkoutRequestContext(a.f.ctx), tc.actor, a.order.ID, "deny:"+uuid.NewString()); !errors.Is(err, tc.want) {
				t.Fatalf("adoption authority: %v, want %v", err, tc.want)
			}
		})
	}
	var sessions int64
	if err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1", a.f.tenant).Scan(&sessions)
	}); err != nil || sessions != 0 || a.payment.creates.Load() != 0 || a.orders.creates.Load() != 0 || a.f.provider.captures.Load() != 0 || a.f.provider.cancels.Load() != 0 {
		t.Fatalf("denied adoption created effects: sessions=%d creates=%d recreates=%d captures=%d cancels=%d %v", sessions, a.payment.creates.Load(), a.orders.creates.Load(), a.f.provider.captures.Load(), a.f.provider.cancels.Load(), err)
	}
}

func TestCheckoutPGAdoptExistingOrderExpiredHoldNeverCaptures(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	start := a.adopt(t, "expired:"+uuid.NewString())
	if err := a.f.runner.InTenant(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_type='order' AND owner_id=$2", a.f.tenant, a.order.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	event := a.commandEvent(t, start)
	a.confirm(t, event, "failed")
	w := a.f.witness(t, a.order.ID)
	if w.state != "cancelled" || w.phase != "failed" || w.onHand != 10 || w.reserved != 0 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 || w.cancelEvents != 1 || a.f.provider.captures.Load() != 0 || a.f.provider.cancels.Load() != 1 {
		t.Fatalf("adoption expired hold effects: %+v captures=%d cancels=%d", w, a.f.provider.captures.Load(), a.f.provider.cancels.Load())
	}
}
