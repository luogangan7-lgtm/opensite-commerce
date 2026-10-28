// Responsibility: exercise customer checkout and staff identity composition through bootstrap.New.
// Dependencies: public customer/checkout/payment contracts, production HTTP admission and httptest.
// Caller: GitHub CI; no database, network, mail or private authentication implementation.
// Invariants: live customer sessions and signed CSRF authorize checkout; staff actors survive wrapping.
package bootstrap_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
)

const checkoutWiringTenant = "11111111-1111-4111-8111-111111111111"
const checkoutWiringOtherTenant = "33333333-3333-4333-8333-333333333333"
const checkoutWiringCustomer = "22222222-2222-4222-8222-222222222222"
const checkoutWiringOrigin = "https://shop.example.test"
const checkoutWiringBody = `{"accepted_total":{"amount_minor":160,"currency":"USD"},"cart_id":"44444444-4444-4444-8444-444444444444","shipping_address":{"country":"US","address1":"Fixture road","city":"Fixture city","postal_code":"00001"}}`

type checkoutWiringCustomerPort struct {
	wiringCustomer
	sessions map[string]customercontract.Session
	calls    int
	tenant   string
	token    string
	failure  error
}

func (p *checkoutWiringCustomerPort) Current(_ context.Context, tenant, token string) (customercontract.Session, error) {
	p.calls++
	p.tenant, p.token = tenant, token
	if p.failure != nil {
		return customercontract.Session{}, p.failure
	}
	session, ok := p.sessions[tenant]
	if !ok || token != p.wiringCustomer.token {
		return customercontract.Session{}, customercontract.ErrUnauthorized
	}
	return session, nil
}

func TestCustomerInvalidCookieUsesGuestCheckoutCapability(t *testing.T) {
	for _, mode := range []string{"expired", "other_tenant", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			handler, port, service, stale := checkoutWiringFixture(t)
			if mode == "unavailable" {
				port.failure = customercontract.ErrUnavailable
			} else {
				if mode == "other_tenant" {
					session := port.sessions[checkoutWiringTenant]
					session.TenantID = checkoutWiringOtherTenant
					port.sessions[checkoutWiringOtherTenant] = session
				}
				delete(port.sessions, checkoutWiringTenant)
			}
			// The anonymous session challenge is the guest's CSRF proof. The
			// unrelated stale customer cookie cannot invalidate that capability.
			challengeRequest := httptest.NewRequest(http.MethodGet, checkoutWiringOrigin+"/api/v1/t/"+checkoutWiringTenant+"/customer/session", nil)
			challenge := httptest.NewRecorder()
			handler.ServeHTTP(challenge, challengeRequest)
			var envelope struct {
				CSRF string `json:"csrf_token"`
			}
			if challenge.Code != 200 || json.Unmarshal(challenge.Body.Bytes(), &envelope) != nil {
				t.Fatal("guest challenge unavailable")
			}
			guestRaw := make([]byte, 32)
			if _, err := rand.Read(guestRaw); err != nil {
				t.Fatal(err)
			}
			guest := base64.RawURLEncoding.EncodeToString(guestRaw)
			body := strings.TrimSuffix(checkoutWiringBody, "}") + `,"buyer_contact":{"email":"buyer@example.test"}}`
			r := checkoutWiringRequest(checkoutWiringTenant, envelope.CSRF, stale)
			r.Body = io.NopCloser(strings.NewReader(body))
			r.Header.Set("X-Cart-Token", guest)
			for _, cookie := range challenge.Result().Cookies() {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			cleared := false
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == customerhttp.SessionCookie && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			if mode == "unavailable" {
				if w.Code != 503 || cleared || service.calls != 0 {
					t.Fatal("CUSTOMER_CHECKOUT_OUTAGE_RED: store failure downgraded identity")
				}
				return
			}
			digest := sha256.Sum256([]byte(guest))
			if w.Code != 201 || service.calls != 1 || service.actor.Kind() != "guest" || service.actor.ID() != hex.EncodeToString(digest[:]) || !cleared {
				t.Fatalf("CUSTOMER_GUEST_CHECKOUT_RED: status=%d start=%d clear=%v", w.Code, service.calls, cleared)
			}
		})
	}
}

func TestCustomerGuestFallbackRequiresIndependentCSRFAndCartToken(t *testing.T) {
	handler, port, service, stale := checkoutWiringFixture(t)
	delete(port.sessions, checkoutWiringTenant)
	challenge := httptest.NewRecorder()
	handler.ServeHTTP(challenge, httptest.NewRequest(http.MethodGet, checkoutWiringOrigin+"/api/v1/t/"+checkoutWiringTenant+"/customer/session", nil))
	var envelope struct {
		CSRF string `json:"csrf_token"`
	}
	if challenge.Code != 200 || json.Unmarshal(challenge.Body.Bytes(), &envelope) != nil || envelope.CSRF == "" {
		t.Fatal("guest challenge unavailable")
	}
	for _, tc := range []struct {
		name   string
		status int
		mutate func(*http.Request)
	}{
		{"bad_csrf", 403, func(r *http.Request) { r.Header.Set("X-CSRF-Token", "invalid") }},
		{"missing_challenge", 403, func(r *http.Request) { r.Header.Del("Cookie"); r.AddCookie(stale) }},
		{"missing_cart_token", 401, func(r *http.Request) { r.Header.Del("X-Cart-Token") }},
		{"invalid_cart_token", 404, func(r *http.Request) { r.Header.Set("X-Cart-Token", "invalid") }},
		{"duplicate_cart_token", 404, func(r *http.Request) { r.Header.Add("X-Cart-Token", stale.Value) }},
		{"foreign_origin", 403, func(r *http.Request) { r.Header.Set("Origin", "https://foreign.example.test") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkoutWiringRequest(checkoutWiringTenant, envelope.CSRF, stale)
			r.Header.Set("X-Cart-Token", stale.Value)
			for _, cookie := range challenge.Result().Cookies() {
				r.AddCookie(cookie)
			}
			tc.mutate(r)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || service.calls != 0 {
				t.Fatalf("guest fallback admitted without independent proof: status=%d want=%d start=%d", w.Code, tc.status, service.calls)
			}
		})
	}
}

type checkoutWiringService struct {
	calls int
	actor checkoutcontract.AuthorizedActor
	input checkoutcontract.StartInput
}

func (s *checkoutWiringService) Start(_ context.Context, actor checkoutcontract.AuthorizedActor, input checkoutcontract.StartInput) (checkoutcontract.Result, error) {
	s.calls++
	s.actor, s.input = actor, input
	return checkoutcontract.Result{CheckoutID: "55555555-5555-4555-8555-555555555555", OrderID: "66666666-6666-4666-8666-666666666666", State: "pending", AmountMinor: 1200, Currency: "USD", OrderVersion: 1, Payment: checkoutcontract.Intent{ProviderRef: "checkout-fixture", Status: payment.StatusRequiresAction, NextActionKind: "redirect", NextActionURL: "https://pay.example.invalid/fixture"}}, nil
}
func (*checkoutWiringService) Confirm(context.Context, payment.VerifiedEvent) (checkoutcontract.Result, error) {
	return checkoutcontract.Result{}, checkoutcontract.ErrUnavailable
}
func (*checkoutWiringService) Cancel(context.Context, checkoutcontract.AuthorizedActor, checkoutcontract.CancelInput) (checkoutcontract.Result, error) {
	return checkoutcontract.Result{}, checkoutcontract.ErrUnavailable
}

func checkoutWiringFixture(t *testing.T) (http.Handler, *checkoutWiringCustomerPort, *checkoutWiringService, *http.Cookie) {
	t.Helper()
	secret, raw := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("fixture randomness unavailable")
	}
	if _, err := rand.Read(raw); err != nil {
		t.Fatal("fixture randomness unavailable")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	port := &checkoutWiringCustomerPort{wiringCustomer: wiringCustomer{token: token}, sessions: map[string]customercontract.Session{checkoutWiringTenant: {ID: checkoutWiringCustomer, TenantID: checkoutWiringTenant, CustomerID: checkoutWiringCustomer, Email: "buyer@example.test", ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}}}
	service := &checkoutWiringService{}
	server, err := bootstrap.New(bootstrap.Config{Customer: port, CustomerOrigin: checkoutWiringOrigin, CustomerSecret: secret, Authenticator: rejectStaffAuth{t: t}, Checkout: &checkouthttp.Config{Service: service, Origin: checkoutWiringOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler, port, service, &http.Cookie{Name: customerhttp.SessionCookie, Value: token}
}

func checkoutWiringCSRF(t *testing.T, handler http.Handler, cookie *http.Cookie, tenant string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, checkoutWiringOrigin+"/api/v1/t/"+tenant+"/customer/session", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var envelope struct {
		CSRF string `json:"csrf_token"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.CSRF == "" {
		t.Fatal("live customer session did not issue signed CSRF")
	}
	return envelope.CSRF
}

func checkoutWiringRequest(tenant, csrf string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodPost, checkoutWiringOrigin+"/api/v1/t/"+tenant+"/checkout", strings.NewReader(checkoutWiringBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "3da81975-3ff1-5ebc-ba1a-ee9e12760c82")
	r.Header.Set("Origin", checkoutWiringOrigin)
	r.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestCustomerCheckoutProductionResolverUsesLiveSession(t *testing.T) {
	handler, port, service, cookie := checkoutWiringFixture(t)
	csrf := checkoutWiringCSRF(t, handler, cookie, checkoutWiringTenant)
	port.calls = 0
	r := checkoutWiringRequest(checkoutWiringTenant, csrf, cookie)
	r.Header.Set("Authorization", "Bearer ignored-staff")
	r.Header.Set("X-Customer-ID", "77777777-7777-4777-8777-777777777777")
	r.Header.Set("X-Tenant-ID", checkoutWiringOtherTenant)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusCreated || port.calls != 1 || port.tenant != checkoutWiringTenant || port.token != cookie.Value || service.calls != 1 || service.actor.Kind() != "customer" || service.actor.TenantID() != checkoutWiringTenant || service.actor.ID() != checkoutWiringCustomer || service.input.CartID != "44444444-4444-4444-8444-444444444444" {
		t.Fatalf("CUSTOMER_CHECKOUT_RESOLVER_RED: status=%d current=%d start=%d", w.Code, port.calls, service.calls)
	}
}

func TestCustomerCheckoutAdmissionFailsClosedBeforeDomain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		mutate func(*http.Request, *checkoutWiringCustomerPort)
		lookup bool
	}{
		{"missing_cookie", 401, func(r *http.Request, _ *checkoutWiringCustomerPort) { r.Header.Del("Cookie") }, false},
		{"spoofed_identity_body", 401, func(r *http.Request, _ *checkoutWiringCustomerPort) {
			r.Header.Del("Cookie")
			r.Header.Set("X-Customer-ID", checkoutWiringCustomer)
			r.Body = io.NopCloser(strings.NewReader(`{"customer_id":"` + checkoutWiringCustomer + `","tenant_id":"` + checkoutWiringTenant + `"}`))
		}, false},
		{"guest_header_without_customer", 403, func(r *http.Request, p *checkoutWiringCustomerPort) {
			r.Header.Del("Cookie")
			r.Header.Set("X-Cart-Token", p.wiringCustomer.token)
		}, false},
		{"bad_csrf", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) { r.Header.Set("X-CSRF-Token", "invalid") }, true},
		{"missing_csrf", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) { r.Header.Del("X-CSRF-Token") }, true},
		{"duplicate_csrf", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) {
			r.Header.Add("X-CSRF-Token", r.Header.Get("X-CSRF-Token"))
		}, true},
		{"foreign_origin", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) {
			r.Header.Set("Origin", "https://foreign.example.test")
		}, false},
		{"missing_origin", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) { r.Header.Del("Origin") }, false},
		{"duplicate_origin", 403, func(r *http.Request, _ *checkoutWiringCustomerPort) { r.Header.Add("Origin", checkoutWiringOrigin) }, false},
		{"revoked_session", 401, func(_ *http.Request, p *checkoutWiringCustomerPort) { delete(p.sessions, checkoutWiringTenant) }, true},
		{"customer_with_guest_header", 404, func(r *http.Request, p *checkoutWiringCustomerPort) {
			r.Header.Set("X-Cart-Token", p.wiringCustomer.token)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, port, service, cookie := checkoutWiringFixture(t)
			csrf := checkoutWiringCSRF(t, handler, cookie, checkoutWiringTenant)
			port.calls = 0
			r := checkoutWiringRequest(checkoutWiringTenant, csrf, cookie)
			tc.mutate(r, port)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			wantCalls := 0
			if tc.lookup {
				wantCalls = 1
			}
			if w.Code != tc.status || service.calls != 0 || port.calls != wantCalls {
				t.Fatalf("CUSTOMER_CHECKOUT_ADMISSION_RED: status=%d want=%d current=%d start=%d", w.Code, tc.status, port.calls, service.calls)
			}
		})
	}
}

type checkoutWiringExplicitResolver struct {
	actor checkoutcontract.AuthorizedActor
	calls int
}

func (r *checkoutWiringExplicitResolver) Resolve(context.Context, *http.Request) (checkoutcontract.AuthorizedActor, error) {
	r.calls++
	return r.actor, nil
}

func TestCustomerCheckoutCompositionPreservesExplicitResolver(t *testing.T) {
	actor, err := checkoutcontract.NewAuthorizedActor(checkoutWiringTenant, "customer", checkoutWiringCustomer)
	if err != nil {
		t.Fatal(err)
	}
	resolver, service := &checkoutWiringExplicitResolver{actor: actor}, &checkoutWiringService{}
	server, err := bootstrap.New(bootstrap.Config{Checkout: &checkouthttp.Config{Origin: checkoutWiringOrigin, Resolver: resolver, Service: service}, Authenticator: rejectStaffAuth{t: t}})
	if err != nil {
		t.Fatal(err)
	}
	// This explicit fixture exercises the injection boundary only; the production
	// customer tests above obtain their CSRF from the real session HTTP handler.
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, checkoutWiringRequest(checkoutWiringTenant, "explicit-resolver-fixture", nil))
	if w.Code != 201 || resolver.calls != 1 || service.calls != 1 || service.actor != actor {
		t.Fatalf("explicit checkout resolver changed: status=%d resolve=%d start=%d", w.Code, resolver.calls, service.calls)
	}
}

func TestCustomerCheckoutTenantBoundCSRFAndSessionLookup(t *testing.T) {
	handler, port, service, cookie := checkoutWiringFixture(t)
	csrf := checkoutWiringCSRF(t, handler, cookie, checkoutWiringTenant)
	// Keep the target session live to isolate tenant-bound CSRF from invalid
	// bearer fallback. The original tenant's MAC still cannot admit this write.
	session := port.sessions[checkoutWiringTenant]
	session.TenantID = checkoutWiringOtherTenant
	port.sessions[checkoutWiringOtherTenant] = session
	port.calls = 0
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, checkoutWiringRequest(checkoutWiringOtherTenant, csrf, cookie))
	if w.Code != 403 || port.calls != 1 || service.calls != 0 {
		t.Fatalf("CUSTOMER_CHECKOUT_TENANT_MAC_RED: status=%d current=%d start=%d", w.Code, port.calls, service.calls)
	}
	// Mint a valid target-tenant MAC through the real public route while this
	// controlled fixture session is live, then revoke its target-tenant mapping.
	// Without a guest capability an invalid bearer grants no checkout access.
	otherCSRF := checkoutWiringCSRF(t, handler, cookie, checkoutWiringOtherTenant)
	delete(port.sessions, checkoutWiringOtherTenant)
	port.calls = 0
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, checkoutWiringRequest(checkoutWiringOtherTenant, otherCSRF, cookie))
	if w.Code != 401 || port.calls != 1 || port.tenant != checkoutWiringOtherTenant || service.calls != 0 {
		t.Fatalf("CUSTOMER_CHECKOUT_TENANT_LOOKUP_RED: status=%d current=%d start=%d", w.Code, port.calls, service.calls)
	}
}

type checkoutWiringStaff struct{ authentications, projections int }

func (s *checkoutWiringStaff) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	s.authentications++
	return ctx, nil
}
func (s *checkoutWiringStaff) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	s.projections++
	return httpapi.Actor{Kind: "user", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, nil
}

func TestCheckoutCompositionPreservesCMSAndStaffActorResolver(t *testing.T) {
	for _, scope := range []string{"cms:write", "orders:read"} {
		t.Run(scope, func(t *testing.T) {
			staff := &checkoutWiringStaff{}
			route := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "checkoutStaffProjectionProbe", Method: http.MethodGet, Pattern: "/api/v1/t/{tenant_id}/checkout-staff-probe", Access: httpapi.AdminOrAPIKey, Scope: scope}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actor, err := httpapi.AuthorizedActor(r.Context())
				if err != nil || actor.Kind != "user" || actor.ID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			server, err := bootstrap.New(bootstrap.Config{Authenticator: staff, Checkout: &checkouthttp.Config{}, Routes: []httpapi.Route{route}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/t/"+checkoutWiringTenant+"/checkout-staff-probe", nil))
			if w.Code != http.StatusNoContent || staff.authentications != 1 || staff.projections != 1 {
				t.Fatalf("CHECKOUT_STAFF_ACTOR_FORWARD_RED: status=%d authenticate=%d project=%d", w.Code, staff.authentications, staff.projections)
			}
		})
	}
}
