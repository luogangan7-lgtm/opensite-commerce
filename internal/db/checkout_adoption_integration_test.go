//go:build pgx

// Responsibility: require production checkout adoption of an already sealed
// customer order, then exercise the same signed Confirm and durable effects.
// Dependencies: existing checkout/order PG fixtures and neutral Payment probe.
// Callers: CI pgx gate after copying this file to internal/db. Invariants: no
// Cart, Rules, order recreation, provider IO in TX, or test-owned payment state.
package db_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
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
	var customer, orderID, currency string
	var amount, net, tax, shipping, version, orderCount, sessions int64
	var noCartID, noCartVersion bool
	if err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.actor_id,s.order_id::text,s.currency,s.amount_minor,s.items_net_minor,s.tax_minor,s.shipping_minor,s.payable_version,
		 s.cart_id IS NULL,s.cart_version IS NULL,
		 (SELECT count(*) FROM app.orders WHERE tenant_id=$1),
		 (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1)
		 FROM app.checkout_sessions s WHERE s.tenant_id=$1 AND s.id=$2`, a.f.tenant, r.CheckoutID).Scan(&customer, &orderID, &currency, &amount, &net, &tax, &shipping, &version, &noCartID, &noCartVersion, &orderCount, &sessions)
	}); err != nil {
		t.Fatal(err)
	}
	if customer != a.f.actor.ID() || orderID != a.order.ID || currency != "USD" || amount != 270 || net != 200 || tax != 20 || shipping != 50 || version != 1 || !noCartID || !noCartVersion || orderCount != 1 || sessions != 1 || a.orders.creates.Load() != 0 {
		t.Fatalf("adoption snapshot owner=%s order=%s %s amount=%d net=%d tax=%d shipping=%d v=%d nullCart=%v/%v orders=%d sessions=%d recreates=%d", customer, orderID, currency, amount, net, tax, shipping, version, noCartID, noCartVersion, orderCount, sessions, a.orders.creates.Load())
	}
}

// The event comes from the neutral provider's actual durable-operation result;
// the production signature verifier admits its wire envelope before Confirm.
func (a checkoutAdoptionPG) signedEvent(t *testing.T, r checkoutcontract.Result) (http.Handler, []byte, string) {
	t.Helper()
	router, key := checkoutHTTPRouter(t, a.f)
	for _, e := range a.f.provider.fake.Events() {
		if e.OrderID != r.OrderID || e.Status != payment.StatusAuthorized {
			continue
		}
		raw, err := json.Marshal(map[string]any{"event_id": e.EventID, "tenant_id": e.TenantID, "order_id": e.OrderID, "order_version": e.OrderVersion,
			"provider_ref": e.ProviderRef, "status": string(e.Status), "amount_minor": e.Amount.Amount, "currency": e.Amount.Currency})
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(raw)
		return router, raw, hex.EncodeToString(mac.Sum(nil))
	}
	t.Fatal("provider did not emit an authorized event for the adopted order")
	return nil, nil, ""
}

func (a checkoutAdoptionPG) deliver(t *testing.T, router http.Handler, raw []byte, signature, state string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, checkouthttp.FakePattern, strings.NewReader(string(raw))).WithContext(checkoutRequestContext(a.f.ctx))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fake-Signature", signature)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var result struct{ State string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != state {
		t.Fatalf("signed Confirm expected %s: %d %s", state, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestCheckoutPGAdoptExistingCustomerOrderPaidBalancedReplay(t *testing.T) {
	a := newCheckoutAdoptionPG(t)
	key := "adopt:" + uuid.NewString()
	start := a.adopt(t, key)
	a.assertSnapshot(t, start)
	router, raw, signature := a.signedEvent(t, start)
	body := a.deliver(t, router, raw, signature, "paid")
	w := a.f.witness(t, a.order.ID)
	if w.state != "paid" || w.phase != "paid" || w.onHand != 8 || w.reserved != 0 || w.journals != 1 || w.entries != 6 || w.debits != 540 || w.credits != 540 || w.receivable != 0 || w.cash != 270 || w.paidEvents != 1 || w.cancelEvents != 0 {
		t.Fatalf("adopted paid effects: %+v", w)
	}
	for range 3 {
		if got := a.deliver(t, router, raw, signature, "paid"); got != body {
			t.Fatalf("signed event replay changed response: %s -> %s", body, got)
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
	router, raw, signature := a.signedEvent(t, start)
	a.deliver(t, router, raw, signature, "failed")
	w := a.f.witness(t, a.order.ID)
	if w.state != "cancelled" || w.phase != "failed" || w.onHand != 10 || w.reserved != 0 || w.journals != 0 || w.entries != 0 || w.paidEvents != 0 || w.cancelEvents != 1 || a.f.provider.captures.Load() != 0 || a.f.provider.cancels.Load() != 1 {
		t.Fatalf("adoption expired hold effects: %+v captures=%d cancels=%d", w, a.f.provider.captures.Load(), a.f.provider.cancels.Load())
	}
}
