//go:build pgx

// Responsibility: drive the real checkout HTTP adapter over real Checkout,
// Orders, Ledger, Fake and PostgreSQL. Dependencies: checkout PG fixture in
// checkout_integration_test.go and the generated strict routes; the resolver is
// an explicit test seam. Invariants: sold-out is a 409 problem, never 500;
// Fake's own canceled callback after Cancel is an idempotent no-op; address
// limits are OpenAPI characters on the actual HTTP -> Start path.
package db_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

func checkoutHTTPRouter(t *testing.T, f *checkoutPGFixture) (*httpapi.Router, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	verifier, err := checkouthttp.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	h, err := checkouthttp.New(checkouthttp.Config{Service: f.service, Resolver: checkoutHTTPResolver{f.actor}, Origin: checkoutHTTPOrigin, FakeEnabled: true, Environment: "test", Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.New(h.Authenticator(nil), nil)
	for _, route := range h.Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return router, key
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
	router, _ := checkoutHTTPRouter(t, f)
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
	router, _ := checkoutHTTPRouter(t, f)
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
	router, _ := checkoutHTTPRouter(t, f)
	if w := checkoutHTTPStart(router, f, strings.Repeat("路", 500), "addr:"+uuid.NewString()); w.Code != 201 {
		t.Fatalf("500-character address rejected: %d %s", w.Code, w.Body.String())
	}
	checkoutHTTPProblem(t, checkoutHTTPStart(router, f, strings.Repeat("路", 501), "addr:"+uuid.NewString()), 400, "invalid_json")
}

func TestCheckoutPGHTTPFakeCanceledCallbackAfterCancel(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	router, key := checkoutHTTPRouter(t, f)
	ctx := checkoutRequestContext(f.ctx)
	start := f.start(t)
	cancelled, err := f.service.Cancel(ctx, f.actor, checkoutcontract.CancelInput{CheckoutID: start.CheckoutID, IdempotencyKey: "cancel:" + uuid.NewString()})
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
	raw, _ := json.Marshal(map[string]any{"event_id": event.EventID, "tenant_id": event.TenantID, "order_id": event.OrderID, "order_version": event.OrderVersion,
		"provider_ref": event.ProviderRef, "status": string(event.Status), "amount_minor": event.Amount.Amount, "currency": event.Amount.Currency})
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	before := f.witness(t, start.OrderID)
	for i := range 3 {
		q := httptest.NewRequest(http.MethodPost, checkouthttp.FakePattern, strings.NewReader(string(raw)))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("X-Fake-Signature", hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, q)
		var got struct{ State string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != "cancelled" {
			t.Fatalf("delivery %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if after := f.witness(t, start.OrderID); after != before || after.state != "cancelled" || after.journals != 0 || after.onHand != 10 || after.reserved != 0 {
		t.Fatalf("canceled callback changed effects: before %+v after %+v", before, after)
	}
	var receipts int
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM app.checkout_attempts WHERE tenant_id=$1 AND event_kind='payment' AND event_key=$2 AND result IS NOT NULL", f.tenant, event.EventID).Scan(&receipts)
	}); err != nil || receipts != 1 {
		t.Fatalf("canceled callback receipt persisted=%d %v", receipts, err)
	}
	if f.provider.captures.Load() != 0 {
		t.Fatal("canceled callback captured")
	}
}
