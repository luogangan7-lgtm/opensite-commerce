// Responsibilities: exercise conversion transport trust and strict admission.
// Dependencies: quote contract and httptest; callers: focused tests and CI.
// Invariants: rejected requests make no domain calls; wire money stays exact.
package quoteorders_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	qh "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quoteorders"
)

const tenantID = "11111111-1111-1111-1111-111111111111"
const quoteID = "22222222-2222-2222-2222-222222222222"
const customerID = "33333333-3333-3333-3333-333333333333"
const companyID = "44444444-4444-4444-4444-444444444444"
const orderID = "55555555-5555-5555-5555-555555555555"

type fixture struct {
	calls   int
	key, id string
	actor   qc.AuthorizedActor
	fail    error
	ref     string
	view    *qc.Conversion
}

func (f *fixture) Convert(ctx context.Context, id, key string) (qc.Conversion, error) {
	f.key = key
	return f.GetConversion(ctx, id)
}
func (f *fixture) GetConversion(ctx context.Context, id string) (qc.Conversion, error) {
	f.calls++
	f.id = id
	f.actor, _ = qc.ActorFromContext(ctx)
	if f.view != nil {
		return *f.view, f.fail
	}
	return qc.Conversion{QuoteID: quoteID, OrderID: orderID, CompanyID: companyID, CustomerID: customerID, IdempotencyKey: "private-key", QuoteVersion: 3, OrderNumber: 7, Amount: money.Money{Amount: 9007199254740993, Currency: "USD"}, State: "pending_payment", ProviderRef: f.ref, CreatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, f.fail
}

type authenticator struct {
	kind, tenant, company string
	fail                  error
	nilContext            bool
	specs                 *[]httpapi.RouteSpec
}

func (a authenticator) Authenticate(ctx context.Context, _ *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	if a.specs != nil {
		*a.specs = append(*a.specs, s)
	}
	if a.nilContext {
		return nil, a.fail
	}
	if a.fail != nil {
		return nil, a.fail
	}
	i := qc.ActorIdentity{TenantID: tenantID}
	if a.tenant != "" {
		i.TenantID = a.tenant
	}
	switch a.kind {
	case "staff":
		i.StaffID = customerID
	case "customer":
		i.CustomerID = customerID
		i.CompanyID = companyID
		if a.company != "" {
			i.CompanyID = a.company
		}
	default:
		return ctx, nil
	}
	return qc.WithTrustedActor(ctx, i), nil
}
func router(t *testing.T, f *fixture, staff, buyer httpapi.Authenticator) http.Handler {
	t.Helper()
	r := httpapi.New(dispatchAuth{staff: staff, buyer: buyer}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range qh.New(f).Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

type dispatchAuth struct{ staff, buyer httpapi.Authenticator }

func (a dispatchAuth) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	chosen := a.staff
	if s.Access == httpapi.BuyerCompany {
		chosen = a.buyer
	}
	if chosen == nil {
		return (httpapi.DenyAuthenticator{}).Authenticate(ctx, r, s)
	}
	return chosen.Authenticate(ctx, r, s)
}
func request(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://buyer.example")
	r.Header.Set("X-CSRF-Token", "verified-session-csrf")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.Header.Set("X-Customer-ID", customerID)
	r.Header.Set("X-Company-ID", companyID)
	r.Header.Set("Authorization", "Bearer fake")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func path(suffix string) string { return "/api/v1/t/" + tenantID + "/quotes/" + quoteID + suffix }
func assertStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
}

func TestQuoteOrderTrustedBuyerDefaultDeny(t *testing.T) {
	for _, buyer := range []httpapi.Authenticator{nil, authenticator{}, authenticator{kind: "staff"}, authenticator{kind: "customer", nilContext: true}, authenticator{kind: "customer", fail: apperrors.New(401, "unauthenticated", "Denied")}, authenticator{kind: "customer", company: "not-uuid"}} {
		f := &fixture{}
		w := request(router(t, f, authenticator{kind: "staff"}, buyer), "POST", path("/orders"), "{}", "k")
		assertStatus(t, w, 401)
		if f.calls != 0 {
			t.Fatal("untrusted buyer reached domain")
		}
	}
}
func TestQuoteOrderSuccessUsesActorAndExactMoney(t *testing.T) {
	f := &fixture{ref: "pi_test_receipt"}
	h := router(t, f, authenticator{kind: "staff"}, authenticator{kind: "customer"})
	for _, x := range []struct {
		method, suffix, body, key string
		status                    int
	}{{"POST", "/orders", "{}", "a", 201}, {"POST", "/orders", "{}", "a", 201}, {"GET", "/order-conversion", "", "", 200}} {
		w := request(h, x.method, path(x.suffix), x.body, x.key)
		assertStatus(t, w, x.status)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("conversion response cacheable")
		}
		var dto map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		if dto["amount"] != "9007199254740993" || dto["quote_version"] != "3" || dto["order_number"] != "7" || dto["quote_id"] != quoteID || dto["order_id"] != orderID || dto["provider_ref"] != "pi_test_receipt" {
			t.Fatalf("incorrect receipt: %+v", dto)
		}
		for _, field := range []string{"customer_id", "company_id", "idempotency_key"} {
			if _, ok := dto[field]; ok {
				t.Fatalf("private field %s leaked", field)
			}
		}
	}
	if f.id != quoteID || f.key != "a" || f.actor.Kind() != "staff" {
		t.Fatal("domain arguments lost")
	}
}
func TestQuoteOrderBuyerTenantAndCompanyIsolation(t *testing.T) {
	f := &fixture{}
	h := router(t, f, nil, authenticator{kind: "customer", tenant: orderID})
	assertStatus(t, request(h, "POST", path("/orders"), "{}", "k"), 404)
	if f.calls != 0 {
		t.Fatal("foreign tenant reached domain")
	}
	f = &fixture{fail: qc.ErrNotFound}
	h = router(t, f, nil, authenticator{kind: "customer", company: orderID})
	assertStatus(t, request(h, "POST", path("/orders"), "{}", "k"), 404)
	if f.calls != 1 || f.actor.CompanyID() != orderID || f.actor.ID() != customerID {
		t.Fatal("company authority replaced by client headers")
	}
}
func TestQuoteOrderSellerAuthAndScope(t *testing.T) {
	for _, staff := range []httpapi.Authenticator{nil, authenticator{}, authenticator{kind: "customer"}} {
		f := &fixture{}
		assertStatus(t, request(router(t, f, staff, nil), "GET", path("/order-conversion"), "", ""), 401)
		if f.calls != 0 {
			t.Fatal("nonstaff read")
		}
	}
	f := &fixture{}
	assertStatus(t, request(router(t, f, authenticator{kind: "staff", tenant: orderID}, nil), "GET", path("/order-conversion"), "", ""), 404)
	if f.calls != 0 {
		t.Fatal("foreign seller read")
	}
	var specs []httpapi.RouteSpec
	f = &fixture{}
	assertStatus(t, request(router(t, f, authenticator{kind: "staff", specs: &specs}, nil), "GET", path("/order-conversion"), "", ""), 200)
	if len(specs) != 1 || specs[0].Access != httpapi.AdminOrAPIKey || specs[0].Scope != "quotes:read" {
		t.Fatalf("seller auth scope: %+v", specs)
	}
}
func TestQuoteOrderStrictAdmission(t *testing.T) {
	for _, body := range []string{`{"amount":"1"}`, `{"price":1}`, `{"company_id":"` + companyID + `"}`, `{"customer_id":"` + customerID + `"}`, `{"x":1,"x":2}`, `{} {}`, `null`, `[]`, ``} {
		f := &fixture{}
		assertStatus(t, request(router(t, f, nil, authenticator{kind: "customer"}), "POST", path("/orders"), body, "k"), 400)
		if f.calls != 0 {
			t.Fatalf("bad body reached domain: %s", body)
		}
	}
	for _, key := range []string{"", " k", "k ", "a b", "a\tb", "é", strings.Repeat("a", 129)} {
		f := &fixture{}
		assertStatus(t, request(router(t, f, nil, authenticator{kind: "customer"}), "POST", path("/orders"), "{}", key), 400)
		if f.calls != 0 {
			t.Fatal("bad key reached domain")
		}
	}
	f := &fixture{}
	h := router(t, f, nil, authenticator{kind: "customer"})
	r := httptest.NewRequest("POST", path("/orders"), strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://buyer.example")
	r.Header.Set("X-CSRF-Token", "verified")
	r.Header.Add("Idempotency-Key", "a")
	r.Header.Add("Idempotency-Key", "b")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertStatus(t, w, 400)
	if f.calls != 0 {
		t.Fatal("duplicate keys reached domain")
	}
	for _, replace := range []string{"BAD", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"} {
		for _, param := range []string{tenantID, quoteID} {
			f := &fixture{}
			assertStatus(t, request(router(t, f, nil, authenticator{kind: "customer"}), "POST", strings.Replace(path("/orders"), param, replace, 1), "{}", "k"), 400)
			if f.calls != 0 {
				t.Fatal("bad UUID reached domain")
			}
		}
	}
}
func TestQuoteOrderKeyBoundAndOptionalReference(t *testing.T) {
	f := &fixture{}
	w := request(router(t, f, nil, authenticator{kind: "customer"}), "POST", path("/orders"), "{}", strings.Repeat("a", 128))
	assertStatus(t, w, 201)
	if strings.Contains(w.Body.String(), "provider_ref") || f.actor.Kind() != "customer" || f.actor.CompanyID() != companyID {
		t.Fatal("optional ref or buyer actor incorrect")
	}
}
func TestQuoteOrderDomainErrorMapping(t *testing.T) {
	for _, x := range []struct {
		err    error
		status int
		code   string
	}{{qc.ErrUnauthorized, 401, "unauthenticated"}, {qc.ErrForbidden, 403, "forbidden"}, {qc.ErrNotFound, 404, "not_found"}, {qc.ErrConflict, 409, "quote_conflict"}, {qc.ErrIdempotencyReused, 422, "idempotency_key_reused"}, {qc.ErrInvalid, 422, "validation_failed"}, {qc.ErrUnavailable, 500, "internal_error"}} {
		f := &fixture{fail: x.err}
		w := request(router(t, f, nil, authenticator{kind: "customer"}), "POST", path("/orders"), "{}", "k")
		assertStatus(t, w, x.status)
		var p map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p["code"] != x.code {
			t.Fatalf("code=%v want=%s", p["code"], x.code)
		}
	}
}

func TestQuoteOrderTransportAndUnavailableFailures(t *testing.T) {
	for _, x := range []struct {
		body, media string
		status      int
	}{{"{}", "text/plain", 415}, {strings.Repeat(" ", 1024) + "{}", "application/json", 413}} {
		f := &fixture{}
		h := router(t, f, nil, authenticator{kind: "customer"})
		r := httptest.NewRequest("POST", path("/orders"), strings.NewReader(x.body))
		r.Header.Set("Content-Type", x.media)
		r.Header.Set("Idempotency-Key", "k")
		r.Header.Set("Origin", "https://buyer.example")
		r.Header.Set("X-CSRF-Token", "verified")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertStatus(t, w, x.status)
		if f.calls != 0 {
			t.Fatal("transport rejection reached domain")
		}
	}
	f := &fixture{}
	h := router(t, f, authenticator{fail: apperrors.New(403, "forbidden", "Scope denied")}, nil)
	assertStatus(t, request(h, "GET", path("/order-conversion"), "", ""), 403)
	if f.calls != 0 {
		t.Fatal("denied scope reached domain")
	}
	r := httpapi.New(authenticator{kind: "customer"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range qh.New(nil).Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	assertStatus(t, request(r, "POST", path("/orders"), "{}", "k"), 500)
	for _, bad := range []string{"BAD", "00000000-0000-0000-0000-000000000000"} {
		f := &fixture{}
		assertStatus(t, request(router(t, f, authenticator{kind: "staff"}, nil), "GET", strings.Replace(path("/order-conversion"), quoteID, bad, 1), "", ""), 400)
		if f.calls != 0 {
			t.Fatal("bad seller UUID reached domain")
		}
	}
}

func TestQuoteOrderRejectsInvalidDomainReceipt(t *testing.T) {
	f := &fixture{}
	base, _ := f.GetConversion(qc.WithTrustedActor(context.Background(), qc.ActorIdentity{TenantID: tenantID, CustomerID: customerID, CompanyID: companyID}), quoteID)
	for _, mutate := range []func(*qc.Conversion){func(v *qc.Conversion) { v.Amount.Amount = 0 }, func(v *qc.Conversion) { v.Amount.Currency = "usd" }, func(v *qc.Conversion) { v.State = "approved" }, func(v *qc.Conversion) { v.ProviderRef = strings.Repeat("x", 513) }} {
		bad := base
		mutate(&bad)
		f := &fixture{view: &bad}
		w := request(router(t, f, nil, authenticator{kind: "customer"}), "POST", path("/orders"), "{}", "k")
		assertStatus(t, w, 500)
		if strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), customerID) {
			t.Fatal("invalid receipt leaked private data")
		}
	}
}

func TestQuoteOrderBuyerScopeAndCookieHeaders(t *testing.T) {
	var specs []httpapi.RouteSpec
	f := &fixture{}
	h := router(t, f, nil, authenticator{kind: "customer", specs: &specs})
	assertStatus(t, request(h, "POST", path("/orders"), "{}", "k"), 201)
	if len(specs) != 1 || specs[0].Access != httpapi.BuyerCompany || specs[0].Scope != "quotes:convert" {
		t.Fatalf("buyer authentication must run exactly once with company scope: %+v", specs)
	}
	for _, name := range []string{"Origin", "X-CSRF-Token"} {
		for _, values := range [][]string{nil, {""}, {"a", "b"}} {
			f := &fixture{}
			h := router(t, f, nil, authenticator{kind: "customer"})
			r := httptest.NewRequest("POST", path("/orders"), strings.NewReader("{}"))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "k")
			r.Header.Set("Origin", "https://buyer.example")
			r.Header.Set("X-CSRF-Token", "verified")
			r.Header.Del(name)
			for _, value := range values {
				r.Header.Add(name, value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assertStatus(t, w, 400)
			if f.calls != 0 {
				t.Fatal("missing or ambiguous cookie protection reached domain")
			}
		}
	}
}
