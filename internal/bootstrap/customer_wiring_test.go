// Responsibility: verify the production composition's customer/cart identity seam.
// Dependencies: public ports and httptest; CI executes this without real mail/DB.
// Invariant: staff/client IDs never replace the live tenant customer actor.
package bootstrap_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
)

type wiringCustomer struct {
	session c.Session
	token   string
}

func (w wiringCustomer) Current(_ context.Context, tenant, token string) (c.Session, error) {
	if tenant != w.session.TenantID || token != w.token {
		return c.Session{}, c.ErrUnauthorized
	}
	return w.session, nil
}
func (w wiringCustomer) Register(context.Context, string, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (w wiringCustomer) Verify(context.Context, string, string) error { return c.ErrUnavailable }
func (w wiringCustomer) RequestReset(context.Context, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (w wiringCustomer) Reset(context.Context, string, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (w wiringCustomer) Login(context.Context, string, string, string, netip.Addr, string) (c.Session, error) {
	return c.Session{}, c.ErrUnavailable
}
func (w wiringCustomer) Logout(context.Context, string, string) error { return c.ErrUnavailable }

type wiringCart struct {
	actor        cc.AuthorizedActor
	calls        int
	contextActor c.Actor
}

func (w *wiringCart) Create(ctx context.Context, actor cc.AuthorizedActor, _ string) (cc.Snapshot, error) {
	w.actor = actor
	w.calls++
	w.contextActor, _ = c.ActorFromContext(ctx)
	return cc.Snapshot{}, errors.New("fixture unavailable")
}
func (*wiringCart) Read(context.Context, cc.AuthorizedActor, string) (cc.Snapshot, error) {
	return cc.Snapshot{}, errors.New("fixture unavailable")
}
func (*wiringCart) Change(context.Context, cc.AuthorizedActor, string, string, int64, func(cc.Snapshot) error, func(cc.Snapshot) error) (cc.Snapshot, error) {
	return cc.Snapshot{}, errors.New("fixture unavailable")
}

func TestCustomerProductionCartWiringAndNoStaffFallback(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const customer = "22222222-2222-4222-8222-222222222222"
	const origin = "https://shop.example.test"
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	port := wiringCustomer{token: token, session: c.Session{ID: customer, TenantID: tenant, CustomerID: customer, Email: "buyer@example.test", ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}}
	repo := &wiringCart{}
	server, err := bootstrap.New(bootstrap.Config{Customer: port, CustomerOrigin: origin, CustomerSecret: secret, Cart: cart.New(repo, nil), Authenticator: rejectStaffAuth{t: t}})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: customerhttp.SessionCookie, Value: token}
	get := httptest.NewRequest("GET", origin+"/api/v1/t/"+tenant+"/customer/session", nil)
	get.AddCookie(cookie)
	view := httptest.NewRecorder()
	server.Handler.ServeHTTP(view, get)
	if view.Code != 200 {
		t.Fatal("live customer session route unavailable")
	}
	var envelope struct {
		CSRF string `json:"csrf_token"`
	}
	if err = json.Unmarshal(view.Body.Bytes(), &envelope); err != nil || envelope.CSRF == "" {
		t.Fatal("CSRF missing")
	}
	for _, valid := range []bool{false, true} {
		req := httptest.NewRequest("POST", origin+"/api/v1/t/"+tenant+"/carts", strings.NewReader(`{"currency":"USD"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer ignored-staff")
		req.AddCookie(cookie)
		if valid {
			req.Header.Set("Origin", origin)
			req.Header.Set("X-CSRF-Token", envelope.CSRF)
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if !valid && response.Code != 403 {
			t.Fatal("cookie storefront write bypassed CSRF")
		}
	}
	if repo.calls != 1 || repo.actor.CustomerID != customer || repo.actor.TenantID != tenant || repo.contextActor.CustomerID != customer || len(repo.actor.GuestTokenHash) != 0 {
		t.Fatal("CUSTOMER_CART_ACTOR_WIRING_RED: authenticated customer context lost")
	}
	wrong := httptest.NewRequest("GET", origin+"/api/v1/t/33333333-3333-4333-8333-333333333333/carts/"+customer, nil)
	wrong.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, wrong)
	if response.Code != 404 {
		t.Fatal("cross tenant cookie did not hide cart")
	}
}

func TestCustomerDefaultProductionRoutesFailClosed(t *testing.T) {
	server, err := bootstrap.New(bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/t/11111111-1111-4111-8111-111111111111/customer/session", nil))
	if response.Code != 503 || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal("default customer authentication fabricated")
	}
}

func TestC02cCustomerSessionCannotAuthorizeStaffRoutes(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const customer = "22222222-2222-4222-8222-222222222222"
	const origin = "https://shop.example.test"
	raw, secret := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	port := wiringCustomer{token: token, session: c.Session{ID: customer, TenantID: tenant, CustomerID: customer, Email: "buyer@example.test", ExpiresAt: time.Now().Add(time.Hour), IdleExpiresAt: time.Now().Add(time.Hour)}}
	for _, access := range []httpapi.Access{httpapi.AdminSession, httpapi.APIKey, httpapi.AdminOrAPIKey} {
		t.Run(string(access), func(t *testing.T) {
			called := false
			route := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "customerStaffBoundaryProbe", Method: http.MethodGet, Pattern: "/api/v1/t/{tenant_id}/customer-staff-probe", Access: access, Scope: "catalog:read"}, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})}
			server, err := bootstrap.New(bootstrap.Config{Customer: port, CustomerOrigin: origin, CustomerSecret: secret, Authenticator: httpapi.DenyAuthenticator{}, Routes: []httpapi.Route{route}})
			if err != nil {
				t.Fatal(err)
			}
			// Positive control: this is an actual admitted customer credential.
			request := httptest.NewRequest(http.MethodGet, origin+"/api/v1/t/"+tenant+"/customer/session", nil)
			request.AddCookie(&http.Cookie{Name: customerhttp.SessionCookie, Value: token})
			positive := httptest.NewRecorder()
			server.Handler.ServeHTTP(positive, request)
			if positive.Code != http.StatusOK {
				t.Fatalf("customer fixture not admitted: %d", positive.Code)
			}
			request = httptest.NewRequest(http.MethodGet, origin+"/api/v1/t/"+tenant+"/customer-staff-probe", nil)
			request.AddCookie(&http.Cookie{Name: customerhttp.SessionCookie, Value: token})
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || called {
				t.Fatalf("customer credential crossed staff boundary: status=%d handler=%v", response.Code, called)
			}
		})
	}
}
