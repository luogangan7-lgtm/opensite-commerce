// Responsibility: exercise real generated checkout routes and trust admission.
// Dependencies: httptest, checkout contract and generated JSON; no DB is mocked
// as SQL evidence. Invariants: default deny, actor target match, raw handoff,
// strict JSON and test-only configuration must precede every receiver call.
package checkout_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	payments "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

const tenant = "10000000-0000-4000-8000-000000000001"
const otherTenant = "10000000-0000-4000-8000-000000000002"
const cart = "20000000-0000-4000-8000-000000000001"
const checkoutID = "30000000-0000-4000-8000-000000000001"
const orderID = "40000000-0000-4000-8000-000000000001"
const origin = "https://store.example.invalid"
const body = `{"accepted_total":{"amount_minor":160,"currency":"USD"},"cart_id":"` + cart + `","buyer_contact":{"email":"guest@example.invalid"},"shipping_address":{"country":"US","address1":"Fixture road","city":"Fixture city","postal_code":"00001"}}`
const eventBody = `{"event_id":"evt-fixture","account_id":"fake-account-fixture","order_id":"` + orderID + `","order_version":1,"provider_ref":"fake-fixture","status":"captured","amount_minor":1200,"currency":"USD"}`

type service struct {
	starts, confirms int
	actor            contract.AuthorizedActor
	input            contract.StartInput
	event            payment.VerifiedEvent
	err              error
}

func (s *service) Start(_ context.Context, a contract.AuthorizedActor, i contract.StartInput) (contract.Result, error) {
	s.starts++
	s.actor = a
	s.input = i
	return result(), s.err
}
func (s *service) Confirm(_ context.Context, e payment.VerifiedEvent) (contract.Result, error) {
	s.confirms++
	s.event = e
	return result(), s.err
}
func (*service) Cancel(context.Context, contract.AuthorizedActor, contract.CancelInput) (contract.Result, error) {
	return result(), nil
}
func result() contract.Result {
	return contract.Result{CheckoutID: checkoutID, OrderID: orderID, State: "pending", AmountMinor: 1200, Currency: "USD", OrderVersion: 1, Payment: contract.Intent{ProviderRef: "fake-fixture", Status: payment.StatusRequiresAction, NextActionKind: "redirect", NextActionURL: "https://pay.example.invalid/fixture"}}
}

type resolver struct {
	actor contract.AuthorizedActor
	err   error
	calls *int
}

func (f resolver) Resolve(_ context.Context, r *http.Request) (contract.AuthorizedActor, error) {
	if f.calls != nil {
		(*f.calls)++
	}
	if f.err != nil {
		return contract.AuthorizedActor{}, f.err
	}
	// A real resolver verifies session-bound material and capability. This is an
	// explicit HTTP seam fixture, never production authentication evidence.
	if r.Header.Get("X-CSRF-Token") != "fixture-session-csrf" {
		return contract.AuthorizedActor{}, apperrors.New(403, "forbidden", "Invalid CSRF material")
	}
	return f.actor, nil
}
func actor(t *testing.T, target, kind string) contract.AuthorizedActor {
	t.Helper()
	id := cart
	if kind == "guest" {
		h := sha256.Sum256([]byte("nonsecret guest capability fixture"))
		id = hex.EncodeToString(h[:])
	}
	a, err := contract.NewAuthorizedActor(target, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func router(t *testing.T, c checkout.Config) *httpapi.Router {
	t.Helper()
	h, err := checkout.New(c)
	if err != nil {
		t.Fatal(err)
	}
	r := httpapi.New(h.Authenticator(nil), nil)
	for _, route := range h.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func request(r http.Handler, path, content string, headers map[string]string) *httptest.ResponseRecorder {
	q := httptest.NewRequest(http.MethodPost, path, strings.NewReader(content))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Origin", origin)
	q.Header.Set("X-CSRF-Token", "fixture-session-csrf")
	q.Header.Set("Idempotency-Key", "fixture-command-1")
	for k, v := range headers {
		if v == "" {
			q.Header.Del(k)
		} else {
			q.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	return w
}
func checkProblem(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("got %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var p struct {
		Status int
		Code   string
	}
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Status != status || p.Code == "" {
		t.Fatalf("invalid problem %s", w.Body.String())
	}
}

// authenticatedStaff is an explicit admission fixture. It accepts only its
// staff cookie and projects a stable actor after authentication; it never puts
// private Router authorization keys into the request context.
type authenticatedStaff struct {
	authentications int
	projections     int
	projectionErr   error
}

func (s *authenticatedStaff) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	cookie, err := r.Cookie("admin_session")
	if err != nil || cookie.Value != "fixture-staff-session" || spec.Access != httpapi.AdminSession || spec.Scope != "fulfillment:write" {
		return nil, apperrors.New(401, "unauthenticated", "Staff fixture session required")
	}
	s.authentications++
	return ctx, nil
}

func (s *authenticatedStaff) ResolveActor(_ context.Context, _ *http.Request, _ httpapi.RouteSpec) (httpapi.Actor, error) {
	s.projections++
	if s.projectionErr != nil {
		return httpapi.Actor{}, s.projectionErr
	}
	return httpapi.Actor{Kind: "user", ID: cart}, nil
}

func TestCheckoutAuthenticatorPreservesAuthenticatedStaffActor(t *testing.T) {
	h, err := checkout.New(checkout.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// The control proves the authenticated fixture works through the same Router.
	// Wrapping it must preserve the actor required by non-checkout staff writes.
	for _, wrapped := range []bool{false, true} {
		staff := &authenticatedStaff{}
		var auth httpapi.Authenticator = staff
		if wrapped {
			auth = h.Authenticator(auth)
		}
		r := httpapi.New(auth, nil)
		reached := false
		spec := httpapi.RouteSpec{OperationID: "staffActorProbe", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/staff-actor-probe", Access: httpapi.AdminSession, Scope: "fulfillment:write"}
		if err := r.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			reached = true
			gotTenant, err := httpapi.AuthorizedTenant(req.Context())
			if err != nil || gotTenant != tenant {
				t.Errorf("wrapped=%v tenant=%q error=%v", wrapped, gotTenant, err)
				return
			}
			gotActor, err := httpapi.AuthorizedActor(req.Context())
			if err != nil {
				httpapi.WriteProblem(w, req, err)
				return
			}
			if gotActor != (httpapi.Actor{Kind: "user", ID: cart}) {
				t.Errorf("wrapped=%v wrong staff actor: %+v", wrapped, gotActor)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})); err != nil {
			t.Fatal(err)
		}
		q := httptest.NewRequest(http.MethodPost, "/api/v1/t/"+tenant+"/staff-actor-probe", nil)
		q.AddCookie(&http.Cookie{Name: "admin_session", Value: "fixture-staff-session"})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		if !reached || staff.authentications != 1 {
			t.Fatalf("wrapped=%v valid staff did not authenticate: reached=%v calls=%d", wrapped, reached, staff.authentications)
		}
		if w.Code != http.StatusNoContent || staff.projections != 1 {
			t.Fatalf("wrapped=%v authenticated staff actor lost: status=%d projections=%d body=%s", wrapped, w.Code, staff.projections, w.Body.String())
		}
	}
}

func TestCheckoutAuthenticatorActorProjectionBoundary(t *testing.T) {
	h, err := checkout.New(checkout.Config{})
	if err != nil {
		t.Fatal(err)
	}
	projectionErr := errors.New("staff projection rejected")
	for _, tc := range []struct {
		name      string
		spec      httpapi.RouteSpec
		wantErr   error
		wantCalls int
	}{
		{"exact checkout", httpapi.RouteSpec{OperationID: checkout.StartOperation, Method: http.MethodPost, Pattern: checkout.StartPattern, Access: httpapi.Storefront}, nil, 0},
		{"other storefront", httpapi.RouteSpec{OperationID: "storefrontActorProbe", Method: http.MethodGet, Pattern: "/api/v1/t/{tenant_id}/actor-probe", Access: httpapi.Storefront}, nil, 0},
		{"fake public", httpapi.RouteSpec{OperationID: checkout.FakeOperation, Method: http.MethodPost, Pattern: checkout.FakePattern, Access: httpapi.Public}, nil, 0},
		{"staff error", httpapi.RouteSpec{OperationID: "staffActorProbe", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/staff-actor-probe", Access: httpapi.AdminSession, Scope: "fulfillment:write"}, projectionErr, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staff := &authenticatedStaff{projectionErr: projectionErr}
			projection, ok := h.Authenticator(staff).(httpapi.ActorResolver)
			if !ok {
				t.Fatal("checkout wrapper lost actor projection contract")
			}
			// Probe the projection seam directly: Router bypasses it for Public,
			// and storefront admission uses its separate typed capability.
			q := httptest.NewRequest(tc.spec.Method, "/", nil)
			got, err := projection.ResolveActor(q.Context(), q, tc.spec)
			if got != (httpapi.Actor{}) || err != tc.wantErr || staff.projections != tc.wantCalls {
				t.Fatalf("actor=%+v error=%v calls=%d; want absent actor, error=%v calls=%d", got, err, staff.projections, tc.wantErr, tc.wantCalls)
			}
		})
	}
}

func TestCheckoutDefaultDeny(t *testing.T) {
	s := &service{}
	r := router(t, checkout.Config{Service: s})
	checkProblem(t, request(r, "/api/v1/t/"+tenant+"/checkout", body, nil), 401)
	if s.starts != 0 {
		t.Fatal("unconfigured authentication called service")
	}
}
func TestCheckoutRejectsObsoleteAuthSpecs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		access  httpapi.Access
		scope   string
		allowed bool
	}{
		{"storefront", httpapi.Storefront, "", true},
		{"old_internal_service", httpapi.InternalService, "checkout.submit", false},
		{"storefront_with_staff_scope", httpapi.Storefront, "checkout.submit", false},
		{"staff_admin_session", httpapi.AdminSession, "checkout.submit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h, err := checkout.New(checkout.Config{Resolver: resolver{actor: actor(t, tenant, "guest"), calls: &calls}, Origin: origin})
			if err != nil {
				t.Fatal(err)
			}
			actual := h.Routes()[0].Spec
			if actual.Access != httpapi.Storefront || actual.Scope != "" {
				t.Errorf("checkout must register Storefront without staff scope: %+v", actual)
			}
			spec := actual
			spec.Access = tc.access
			spec.Scope = tc.scope
			req := httptest.NewRequest(http.MethodPost, "/api/v1/t/"+tenant+"/checkout", strings.NewReader(body))
			req.SetPathValue("tenant_id", tenant)
			req.Header.Set("Origin", origin)
			req.Header.Set("X-CSRF-Token", "fixture-session-csrf")
			_, err = h.Authenticator(httpapi.DenyAuthenticator{}).Authenticate(req.Context(), req, spec)
			if tc.allowed {
				if err != nil || calls != 1 {
					t.Fatalf("valid Storefront rejected: err=%v resolver_calls=%d", err, calls)
				}
			} else {
				if err == nil || calls != 0 {
					t.Fatalf("obsolete/staff access spec authorized: err=%v resolver_calls=%d", err, calls)
				}
				w := httptest.NewRecorder()
				httpapi.WriteProblem(w, req, err)
				checkProblem(t, w, http.StatusUnauthorized)
			}
		})
	}
}

func TestCheckoutActorAndStrictAdmission(t *testing.T) {
	cases := []struct {
		name, content string
		headers       map[string]string
		status        int
	}{
		{"guest", body, nil, 201},
		{"unknown field", strings.TrimSuffix(body, "}") + `,"tenant_id":"` + otherTenant + `"}`, nil, 400},
		{"second JSON", body + ` {}`, nil, 400},
		{"case variant field", strings.Replace(body, "cart_id", "CART_ID", 1), nil, 400},
		{"wrong media", body, map[string]string{"Content-Type": "text/plain"}, 415},
		{"oversize", strings.Repeat(" ", 256<<10) + body, nil, 413},
		{"missing key", body, map[string]string{"Idempotency-Key": ""}, 400},
		{"forged buyer identity", strings.TrimSuffix(body, "}") + `,"buyer_id":"` + cart + `"}`, nil, 400},
		{"body idempotency", strings.TrimSuffix(body, "}") + `,"idempotency_key":"body-key"}`, nil, 400},
		{"bad origin", body, map[string]string{"Origin": "https://forged.example.invalid"}, 403},
		{"missing csrf", body, map[string]string{"X-CSRF-Token": ""}, 403},
		{"unbound csrf", body, map[string]string{"X-CSRF-Token": "other-material"}, 403},
		{"quote unsupported", strings.TrimSuffix(body, "}") + `,"quote_id":"` + cart + `"}`, nil, 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "guest")}, Origin: origin})
			w := request(r, "/api/v1/t/"+tenant+"/checkout", c.content, c.headers)
			if c.status != 201 {
				checkProblem(t, w, c.status)
				if s.starts != 0 {
					t.Fatal("rejected request called service")
				}
				return
			}
			if w.Code != 201 || s.starts != 1 || s.actor.Kind() != "guest" || s.input.IdempotencyKey != "fixture-command-1" || s.input.BuyerContact.Email != "guest@example.invalid" || s.input.ShippingAddress.Country != "US" {
				t.Fatalf("request failed %d %s input=%+v", w.Code, w.Body.String(), s.input)
			}
			var result httpapi.CheckoutResult
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.CheckoutId.String() != checkoutID || result.AmountMinor != 1200 {
				t.Fatalf("bad generated response %s", w.Body.String())
			}
		})
	}
}
func TestCheckoutRejectsTenantAndInvalidActor(t *testing.T) {
	for _, c := range []struct {
		name   string
		actor  contract.AuthorizedActor
		status int
	}{{"different tenant", actor(t, otherTenant, "guest"), 404}, {"missing actor", contract.AuthorizedActor{}, 401}} {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: c.actor}, Origin: origin})
			checkProblem(t, request(r, "/api/v1/t/"+tenant+"/checkout", body, nil), c.status)
			if s.starts != 0 {
				t.Fatal("untrusted actor reached service")
			}
		})
	}
	s := &service{}
	r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "customer")}, Origin: origin})
	w := request(r, "/api/v1/t/"+tenant+"/checkout", strings.Replace(body, `"buyer_contact":{"email":"guest@example.invalid"},`, "", 1), nil)
	if w.Code != 201 || s.actor.ID() != cart {
		t.Fatal("customer ID must come only from trusted actor")
	}
}
func TestCheckoutCustomerContactAdmission(t *testing.T) {
	noContact := strings.Replace(body, `"buyer_contact":{"email":"guest@example.invalid"},`, "", 1)
	for _, c := range []struct {
		name, kind, content string
		status              int
	}{{"guest missing contact", "guest", noContact, 422}, {"customer no contact", "customer", noContact, 201}, {"customer supplies guest snapshot", "customer", body, 422}} {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, c.kind)}, Origin: origin})
			w := request(r, "/api/v1/t/"+tenant+"/checkout", c.content, nil)
			if c.status != 201 {
				checkProblem(t, w, c.status)
				if s.starts != 0 {
					t.Fatal("invalid actor/contact combination reached service")
				}
				return
			}
			if w.Code != 201 || s.starts != 1 || s.input.BuyerContact.Email != "" || s.input.BuyerContact.Name != "" || s.input.BuyerContact.Phone != "" {
				t.Fatalf("customer guest snapshot must remain empty: %d %+v", w.Code, s.input)
			}
		})
	}
}
func TestCheckoutServiceErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{{contract.ErrInvalid, 422}, {contract.ErrForbidden, 404}, {contract.ErrNotFound, 404}, {contract.ErrKeyReused, 422}, {contract.ErrConflict, 409}, {contract.ErrUnavailable, 503}, {contract.ErrRetry, 503}, {errors.New("private failure diagnostic"), 500},
		// Orders errors reach HTTP verbatim from Start/Confirm and keep their meaning.
		{orders.ErrStockUnavailable, 409}, {orders.ErrReservationInactive, 409}, {orders.ErrConflict, 409}, {orders.ErrInvalid, 422}, {orders.ErrKeyReused, 422}, {orders.ErrNotFound, 404}, {orders.ErrReservationOwner, 404}, {orders.ErrRetry, 503}, {orders.ErrUnavailable, 503}} {
		s := &service{err: c.err}
		r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "guest")}, Origin: origin})
		w := request(r, "/api/v1/t/"+tenant+"/checkout", body, nil)
		checkProblem(t, w, c.status)
		if strings.Contains(w.Body.String(), "private failure") {
			t.Fatal("internal diagnostic exposed")
		}
	}
}

// receiver is a non-degenerate HTTP contract fixture, not signature/SQL proof.
type receiver struct {
	calls     int
	raw       []byte
	signature string
	status    string
	err       error
}

func (r *receiver) Receive(_ context.Context, raw []byte, signature string) (payments.Receipt, error) {
	r.calls++
	r.raw = append([]byte(nil), raw...)
	r.signature = signature
	status := r.status
	if status == "" {
		status = "processed"
	}
	return payments.Receipt{EventID: "evt-fixture", Status: status}, r.err
}

const fixtureSignature = "t=1791900000,v1=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestFakeWebhookProductionForbidden(t *testing.T) {
	for _, mode := range []string{"", "production", "Production", "staging"} {
		if _, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: mode, Events: &receiver{}}); err == nil {
			t.Fatalf("Fake enabled in %q", mode)
		}
	}
	for _, mode := range []string{"test", "development"} {
		if _, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: mode, Events: &receiver{}}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := checkout.NewFakeHMACVerifier(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []checkout.Config{
		{FakeEnabled: true, Environment: "test"},
		{FakeEnabled: true, Environment: "test", Verifier: v},
	} {
		if _, err := checkout.New(c); err == nil {
			t.Fatal("Fake admitted without trusted Payments receiver")
		}
	}
	r := router(t, checkout.Config{})
	checkProblem(t, request(r, checkout.FakePattern, eventBody, nil), 404)
	if _, err := checkout.New(checkout.Config{Resolver: resolver{actor: actor(t, tenant, "guest")}}); err == nil {
		t.Fatal("resolver admitted without exact origin")
	}
}

func TestFakeWebhookRawHandoffAndReceipt(t *testing.T) {
	for _, status := range []string{"pending", "processed"} {
		t.Run(status, func(t *testing.T) {
			s := &service{}
			events := &receiver{status: status}
			r := router(t, checkout.Config{Service: s, FakeEnabled: true, Environment: "test", Events: events})
			raw := " \n" + eventBody + " \t"
			w := request(r, checkout.FakePattern, raw, map[string]string{"X-Fake-Signature": fixtureSignature})
			var receipt struct {
				EventID string `json:"event_id"`
				Status  string `json:"status"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.EventID != "evt-fixture" || receipt.Status != status {
				t.Fatalf("bad receipt %d %s", w.Code, w.Body.String())
			}
			if events.calls != 1 || string(events.raw) != raw || events.signature != fixtureSignature || s.confirms != 0 || s.starts != 0 {
				t.Fatalf("raw handoff changed bytes/signature or issued checkout command: receiver=%+v service=%+v", events, s)
			}
		})
	}
	events := &receiver{}
	h, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: "test", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReceiveFakePaymentEvent(context.Background(), httpapi.ReceiveFakePaymentEventRequestObject{}); err == nil || events.calls != 0 {
		t.Fatal("direct body/context bypassed raw admission")
	}
	if _, err := h.CreateCheckout(context.Background(), httpapi.CreateCheckoutRequestObject{}); err == nil {
		t.Fatal("direct handler bypassed actor proof")
	}
}

func TestFakeWebhookStrictAdmissionBeforeReceive(t *testing.T) {
	for _, c := range []struct {
		name, content, signature, media string
		status                          int
	}{
		{"valid", eventBody, fixtureSignature, "application/json", 200},
		{"refund", strings.Replace(eventBody, "captured", "partially_refunded", 1), fixtureSignature, "application/json", 200},
		{"full refund", strings.Replace(eventBody, "captured", "refunded", 1), fixtureSignature, "application/json", 200},
		{"missing signature", eventBody, "", "application/json", 401},
		{"timestamp free", eventBody, strings.Repeat("a", 64), "application/json", 400},
		{"malformed signature", eventBody, "t=bad,v1=" + strings.Repeat("a", 64), "application/json", 400},
		{"legacy tenant authority", strings.Replace(eventBody, `"account_id":"fake-account-fixture"`, `"tenant_id":"`+tenant+`"`, 1), fixtureSignature, "application/json", 400},
		{"bad account", strings.Replace(eventBody, "fake-account-fixture", "fake/account", 1), fixtureSignature, "application/json", 400},
		{"authorized command", strings.Replace(eventBody, "captured", "authorized", 1), fixtureSignature, "application/json", 400},
		{"unknown", strings.TrimSuffix(eventBody, "}") + `,"untrusted":true}`, fixtureSignature, "application/json", 400},
		{"second", eventBody + ` {}`, fixtureSignature, "application/json", 400},
		{"duplicate", strings.TrimSuffix(eventBody, "}") + `,"amount_minor":1200}`, fixtureSignature, "application/json", 400},
		{"overflow", strings.Replace(eventBody, "1200", "9223372036854775808", 1), fixtureSignature, "application/json", 400},
		{"case variant", strings.Replace(eventBody, "account_id", "ACCOUNT_ID", 1), fixtureSignature, "application/json", 400},
		{"wrong media", eventBody, fixtureSignature, "text/plain", 415},
		{"oversize", strings.Repeat(" ", 64<<10) + eventBody, fixtureSignature, "application/json", 413},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			events := &receiver{}
			r := router(t, checkout.Config{Service: s, FakeEnabled: true, Environment: "test", Events: events})
			w := request(r, checkout.FakePattern, c.content, map[string]string{"X-Fake-Signature": c.signature, "Content-Type": c.media})
			if c.status != 200 {
				checkProblem(t, w, c.status)
				if events.calls != 0 {
					t.Fatal("invalid strict webhook called receiver")
				}
			} else if w.Code != 200 || events.calls != 1 {
				t.Fatalf("valid event not received: %d %s", w.Code, w.Body.String())
			}
			if s.confirms != 0 {
				t.Fatal("webhook issued legacy checkout command")
			}
		})
	}
	events := &receiver{}
	r := router(t, checkout.Config{FakeEnabled: true, Environment: "test", Events: events})
	q := httptest.NewRequest(http.MethodPost, checkout.FakePattern, strings.NewReader(eventBody))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Add("X-Fake-Signature", fixtureSignature)
	q.Header.Add("X-Fake-Signature", fixtureSignature)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	checkProblem(t, w, 401)
	if events.calls != 0 {
		t.Fatal("duplicate signature header called receiver")
	}
}

func TestFakeWebhookReceiverSafeErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{
		{apperrors.New(401, "unauthenticated", "Authentication is required"), 401},
		{apperrors.New(409, "payment_conflict", "Payment event conflict"), 409},
		{apperrors.New(422, "invalid_event", "Invalid payment event"), 422},
		{apperrors.New(503, "payment_unavailable", "private failure diagnostic"), 503},
		{errors.New("private failure diagnostic"), 500},
	} {
		events := &receiver{err: c.err}
		r := router(t, checkout.Config{FakeEnabled: true, Environment: "test", Events: events})
		w := request(r, checkout.FakePattern, eventBody, map[string]string{"X-Fake-Signature": fixtureSignature})
		checkProblem(t, w, c.status)
		if events.calls != 1 || strings.Contains(w.Body.String(), "private failure") {
			t.Fatal("receiver error skipped or diagnostic exposed")
		}
	}
}

func TestFakeWebhookRejectsInvalidReceiverReceipt(t *testing.T) {
	for _, c := range []struct {
		status string
		code   int
	}{{"rejected", 409}, {"unexpected", 500}} {
		events := &receiver{status: c.status}
		r := router(t, checkout.Config{FakeEnabled: true, Environment: "test", Events: events})
		w := request(r, checkout.FakePattern, eventBody, map[string]string{"X-Fake-Signature": fixtureSignature})
		checkProblem(t, w, c.code)
		if events.calls != 1 {
			t.Fatal("receipt error skipped receiver")
		}
	}
}

func TestDuplicateSecurityHeadersRejected(t *testing.T) {
	s := &service{}
	r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "guest")}, Origin: origin})
	for _, header := range []string{"Origin", "X-CSRF-Token", "Idempotency-Key"} {
		q := httptest.NewRequest("POST", "/api/v1/t/"+tenant+"/checkout", strings.NewReader(body))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Origin", origin)
		q.Header.Set("X-CSRF-Token", "fixture-session-csrf")
		q.Header.Set("Idempotency-Key", "fixture-command-1")
		q.Header.Add(header, q.Header.Get(header))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		status := 403
		if header == "Idempotency-Key" {
			status = 400
		}
		checkProblem(t, w, status)
	}
	if s.starts != 0 {
		t.Fatal("duplicate headers reached service")
	}
}

func TestBrowserCompositionRoutesAndStaffAuthority(t *testing.T) {
	h, err := checkout.New(checkout.Config{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"createCheckout": "POST /api/v1/t/{tenant_id}/checkout", "previewCheckout": "POST /api/v1/t/{tenant_id}/checkout/preview", "getCheckout": "GET /api/v1/t/{tenant_id}/checkout/{checkout_id}"}
	for _, route := range h.Routes() {
		spec := route.Spec
		if want[spec.OperationID] != spec.Method+" "+spec.Pattern || spec.Access != httpapi.Storefront || spec.Scope != "" {
			t.Fatalf("unexpected browser route: %+v", spec)
		}
		delete(want, spec.OperationID)

	}
	if len(want) != 0 {
		t.Fatalf("BROWSER_ROUTES_RED: missing %v", want)
	}
}
