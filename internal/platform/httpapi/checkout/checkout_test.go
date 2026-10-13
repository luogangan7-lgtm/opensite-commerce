// Responsibility: exercise real generated checkout routes and trust admission.
// Dependencies: httptest, checkout contract and generated JSON; no DB is mocked
// as SQL evidence. Invariants: default deny, actor target match, raw MAC proof,
// strict JSON and test-only configuration must precede every domain call.
package checkout_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
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
const body = `{"cart_id":"` + cart + `","buyer_contact":{"email":"guest@example.invalid"},"shipping_address":{"country":"US","address1":"Fixture road","city":"Fixture city","postal_code":"00001"}}`
const eventBody = `{"event_id":"evt-fixture","tenant_id":"` + tenant + `","order_id":"` + orderID + `","order_version":1,"provider_ref":"fake-fixture","status":"captured","amount_minor":1200,"currency":"USD"}`

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
}

func (f resolver) Resolve(_ context.Context, r *http.Request) (contract.AuthorizedActor, error) {
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
func TestCheckoutDefaultDeny(t *testing.T) {
	s := &service{}
	r := router(t, checkout.Config{Service: s})
	checkProblem(t, request(r, "/api/v1/t/"+tenant+"/checkout", body, nil), 401)
	if s.starts != 0 {
		t.Fatal("unconfigured authentication called service")
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
	}{{contract.ErrInvalid, 422}, {contract.ErrForbidden, 404}, {contract.ErrNotFound, 404}, {contract.ErrKeyReused, 422}, {contract.ErrConflict, 409}, {contract.ErrUnavailable, 503}, {contract.ErrRetry, 503}, {errors.New("private failure diagnostic"), 500}} {
		s := &service{err: c.err}
		r := router(t, checkout.Config{Service: s, Resolver: resolver{actor: actor(t, tenant, "guest")}, Origin: origin})
		w := request(r, "/api/v1/t/"+tenant+"/checkout", body, nil)
		checkProblem(t, w, c.status)
		if strings.Contains(w.Body.String(), "private failure") {
			t.Fatal("internal diagnostic exposed")
		}
	}
}
func TestFakeWebhookProductionForbidden(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	v, err := checkout.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "production", "Production", "staging"} {
		if _, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: mode, Verifier: v}); err == nil {
			t.Fatalf("Fake enabled in %q", mode)
		}
	}
	for _, mode := range []string{"test", "development"} {
		if _, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: mode, Verifier: v}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := checkout.New(checkout.Config{FakeEnabled: true, Environment: "test"}); err == nil {
		t.Fatal("unsigned Fake admitted")
	}
	r := router(t, checkout.Config{})
	checkProblem(t, request(r, checkout.FakePattern, eventBody, nil), 404)
	if _, err := checkout.New(checkout.Config{Resolver: resolver{actor: actor(t, tenant, "guest")}}); err == nil {
		t.Fatal("resolver admitted without exact origin")
	}
}
func sign(key []byte, raw string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(raw))
	return hex.EncodeToString(m.Sum(nil))
}
func TestFakeWebhookRawSignatureRequired(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	v, err := checkout.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, body, sig string
		status          int
	}{{"missing", eventBody, "", 401}, {"wrong key", eventBody, strings.Repeat("0", 64), 401}, {"tampered bytes", eventBody + " ", sign(key, eventBody), 401}, {"valid", eventBody, sign(key, eventBody), 200}} {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			r := router(t, checkout.Config{Service: s, FakeEnabled: true, Environment: "test", Verifier: v})
			w := request(r, checkout.FakePattern, c.body, map[string]string{"X-Fake-Signature": c.sig})
			if c.status != 200 {
				checkProblem(t, w, c.status)
				if s.confirms != 0 {
					t.Fatal("unverified webhook reached service")
				}
				return
			}
			if w.Code != 200 || s.confirms != 1 || s.event.TenantID != tenant || s.event.OrderID != orderID || s.event.Amount.Amount != 1200 {
				t.Fatalf("event not admitted correctly %d %s", w.Code, w.Body.String())
			}
		})
	}
	h, _ := checkout.New(checkout.Config{Service: &service{}, FakeEnabled: true, Environment: "test", Verifier: v})
	if _, err := h.ReceiveFakePaymentEvent(context.Background(), httpapi.ReceiveFakePaymentEventRequestObject{}); err == nil {
		t.Fatal("direct body/context bypassed signature proof")
	}
	if _, err := h.CreateCheckout(context.Background(), httpapi.CreateCheckoutRequestObject{}); err == nil {
		t.Fatal("direct handler bypassed actor proof")
	}
}

type fixedVerifier struct{ event payment.VerifiedEvent }

func (v fixedVerifier) Verify(context.Context, []byte, string) (payment.VerifiedEvent, error) {
	return v.event, nil
}
func TestFakeWebhookStrictJSONAndProofOnly(t *testing.T) {
	// Fixed trusted event deliberately disagrees with body data. Handler may use
	// only verifier's proof; core will later recheck it against database binding.
	proof := payment.VerifiedEvent{EventID: "verified", TenantID: otherTenant, OrderID: orderID, OrderVersion: 1, ProviderRef: "trusted-provider", Status: payment.StatusCaptured, Amount: money.Money{Amount: 1200, Currency: "USD"}}
	for _, c := range []struct {
		name, content string
		status        int
	}{{"valid", eventBody, 200}, {"unknown", strings.TrimSuffix(eventBody, "}") + `,"untrusted":true}`, 400}, {"second", eventBody + ` {}`, 400}, {"duplicate", strings.TrimSuffix(eventBody, "}") + `,"amount_minor":1200}`, 400}, {"oversize", strings.Repeat(" ", 64<<10) + eventBody, 413}} {
		t.Run(c.name, func(t *testing.T) {
			s := &service{}
			r := router(t, checkout.Config{Service: s, FakeEnabled: true, Environment: "test", Verifier: fixedVerifier{proof}})
			w := request(r, checkout.FakePattern, c.content, map[string]string{"X-Fake-Signature": strings.Repeat("a", 64)})
			if c.status != 200 {
				checkProblem(t, w, c.status)
				if s.confirms != 0 {
					t.Fatal("invalid strict webhook called service")
				}
				return
			}
			if w.Code != 200 || s.event.TenantID != otherTenant || s.event.EventID != "verified" {
				t.Fatal("HTTP decoded body masqueraded as trusted event")
			}
		})
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
