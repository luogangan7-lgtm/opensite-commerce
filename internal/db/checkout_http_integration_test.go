//go:build pgx

// Responsibility: drive the real checkout HTTP adapter over real Checkout,
// Orders, Ledger, Fake and PostgreSQL. Dependencies: checkout PG fixture in
// checkout_integration_test.go, paymentFactFixture and generated strict routes; the resolver is
// an explicit test seam. Invariants: sold-out is a 409 problem, never 500;
// Fake's own canceled callback after Cancel is an idempotent no-op; address
// limits are OpenAPI characters on the actual HTTP -> Start path.
package db_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	paymentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

const checkoutHTTPOrigin = "https://store.example.invalid"

type checkoutHTTPResolver struct {
	actor checkoutcontract.AuthorizedActor
}

func (r checkoutHTTPResolver) Resolve(context.Context, *http.Request) (checkoutcontract.AuthorizedActor, error) {
	return r.actor, nil
}

// Start-only fixtures do not need payment account/signature composition.
func checkoutHTTPRouter(t *testing.T, f *checkoutPGFixture) *httpapi.Router {
	t.Helper()
	return checkoutHTTPConfiguredRouter(t, checkouthttp.Config{Service: f.service, Resolver: checkoutHTTPResolver{f.actor}, Origin: checkoutHTTPOrigin})
}

func checkoutHTTPConfiguredRouter(t *testing.T, config checkouthttp.Config) *httpapi.Router {
	t.Helper()
	h, err := checkouthttp.New(config)
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.New(h.Authenticator(nil), nil)
	for _, route := range h.Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return router
}

func checkoutHTTPStart(router http.Handler, f *checkoutPGFixture, address1, key string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"cart_id": f.cart.cart.ID, "buyer_contact": map[string]string{"email": "buyer@example.test"},
		"shipping_address": map[string]string{"country": "US", "address1": address1, "city": "Test City", "postal_code": "10000"}})
	q := httptest.NewRequest(http.MethodPost, "/api/v1/t/"+f.tenant+"/checkout", strings.NewReader(string(body)))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Origin", checkoutHTTPOrigin)
	q.Header.Set("X-CSRF-Token", "fixture-session-csrf")
	q.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, q)
	return w
}

func checkoutHTTPProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var p struct {
		Status int
		Code   string
	}
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Status != status || p.Code != code {
		t.Fatalf("want %d/%s problem, got %d %s %s", status, code, w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestCheckoutPGHTTPSoldOutIs409(t *testing.T) {
	f := newCheckoutPGFixture(t, 1, 2)
	router := checkoutHTTPRouter(t, f)
	checkoutHTTPProblem(t, checkoutHTTPStart(router, f, "100 Test Avenue", "sold-out:"+uuid.NewString()), 409, "stock_unavailable")
	var onHand, reserved, orders int64
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT on_hand,reserved,(SELECT count(*) FROM app.orders WHERE tenant_id=$1) FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&onHand, &reserved, &orders)
	}); err != nil || onHand != 1 || reserved != 0 || orders != 0 {
		t.Fatalf("sold-out request changed state: on_hand=%d reserved=%d orders=%d %v", onHand, reserved, orders, err)
	}
}

func TestCheckoutPGHTTPLastItemRaceOne201One409(t *testing.T) {
	f := newCheckoutPGFixture(t, 1, 1)
	router := checkoutHTTPRouter(t, f)
	var wg sync.WaitGroup
	codes := make([]*httptest.ResponseRecorder, 2)
	for i := range codes {
		wg.Go(func() { codes[i] = checkoutHTTPStart(router, f, "100 Test Avenue", "last-item:"+uuid.NewString()) })
	}
	wg.Wait()
	created, conflict := 0, 0
	for _, w := range codes {
		switch w.Code {
		case 201:
			created++
		case 409:
			checkoutHTTPProblem(t, w, 409, "stock_unavailable")
			conflict++
		default:
			t.Fatalf("last-item race status %d %s", w.Code, w.Body.String())
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("last-item race created=%d conflict=%d", created, conflict)
	}
}

// Characters, not bytes: 500 CJK characters (1500 bytes) is within the
// OpenAPI address1 maxLength and must succeed through HTTP -> Start.
func TestCheckoutPGHTTPAddressCharacterLimit(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 1)
	router := checkoutHTTPRouter(t, f)
	if w := checkoutHTTPStart(router, f, strings.Repeat("路", 500), "addr:"+uuid.NewString()); w.Code != 201 {
		t.Fatalf("500-character address rejected: %d %s", w.Code, w.Body.String())
	}
	checkoutHTTPProblem(t, checkoutHTTPStart(router, f, strings.Repeat("路", 501), "addr:"+uuid.NewString()), 400, "invalid_json")
}

func TestCheckoutPGHTTPFakeCanceledCallbackAfterCancel(t *testing.T) {
	p := newPaymentFactFixture(t)
	f, start := p.f, p.start
	ctx := checkoutRequestContext(f.ctx)
	// Cancel is the one intended provider command; restore the fact-consumer
	// command tripwire before admitting its callback. No request runs in flight.
	guard := f.provider.before
	f.provider.before = nil
	cancelled, err := f.service.Cancel(ctx, f.actor, checkoutcontract.CancelInput{CheckoutID: start.CheckoutID, IdempotencyKey: "cancel:" + uuid.NewString()})
	f.provider.before = guard
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("Cancel %#v %v", cancelled, err)
	}
	var event *payment.VerifiedEvent
	for _, e := range f.provider.fake.Events() {
		if e.OrderID == start.OrderID && e.Status == payment.StatusCanceled {
			v := payment.VerifiedEvent(e)
			event = &v
		}
	}
	if event == nil {
		t.Fatal("Fake produced no canceled event for the order")
	}
	// Keep the callback sourced from Fake's actual canceled event. Only authority
	// moves from wire tenant_id to the configured provider account.
	raw, err := json.Marshal(map[string]any{"event_id": event.EventID, "account_id": p.account, "order_id": event.OrderID, "order_version": event.OrderVersion,
		"provider_ref": event.ProviderRef, "status": string(event.Status), "amount_minor": event.Amount.Amount, "currency": event.Amount.Currency})
	if err != nil {
		t.Fatal(err)
	}
	router := checkoutHTTPConfiguredRouter(t, checkouthttp.Config{Service: f.service, FakeEnabled: true, Environment: "test", Events: p.module})
	before := f.witness(t, start.OrderID)
	captures, cancels := f.provider.captures.Load(), f.provider.cancels.Load()
	if captures != 0 || cancels != 1 {
		t.Fatalf("Cancel provider effects captures=%d cancels=%d", captures, cancels)
	}
	for i := range 3 {
		q := httptest.NewRequest(http.MethodPost, checkouthttp.FakePattern, strings.NewReader(string(raw)))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("X-Fake-Signature", p.signature(raw, p.at))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, q)
		var got paymentcontract.Receipt
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.EventID != event.EventID || got.Status != "processed" {
			t.Fatalf("delivery %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if after := f.witness(t, start.OrderID); after != before || after.state != "cancelled" || after.journals != 0 || after.onHand != 10 || after.reserved != 0 {
		t.Fatalf("canceled callback changed effects: before %+v after %+v", before, after)
	}
	var receipts, inboxes, events int
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM app.checkout_attempts a JOIN app.payment_events e ON (a.tenant_id,a.event_key)=(e.tenant_id,e.event_key) JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(e.tenant_id,e.id) WHERE a.tenant_id=$1 AND a.event_kind='payment' AND a.result IS NOT NULL AND e.provider_account_id=$3 AND i.provider_event_id=$2),
		 (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider='fake' AND provider_account_id=$3 AND environment='test' AND provider_event_id=$2 AND status='processed'),
		 (SELECT count(*) FROM app.payment_events e JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(e.tenant_id,e.id) WHERE e.tenant_id=$1 AND e.provider_account_id=$3 AND i.provider_event_id=$2 AND e.applied)`, f.tenant, event.EventID, p.account).Scan(&receipts, &inboxes, &events)
	}); err != nil || receipts != 1 || inboxes != 1 || events != 1 {
		t.Fatalf("canceled callback receipts checkout=%d inbox=%d events=%d %v", receipts, inboxes, events, err)
	}
	if p.providerCommands.Load() != 0 || f.provider.captures.Load() != captures || f.provider.cancels.Load() != cancels {
		t.Fatal("canceled callback issued another provider command")
	}
}
