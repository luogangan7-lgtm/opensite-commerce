// Responsibilities: prove only successful private routing stamps stable actors.
// Dependencies: httpapi public API and stdlib; callers: Go security gates.
// Invariants: no header/context fabrication, stale stamp or cross-actor reuse.
package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const actorTenant = "11111111-1111-4111-8111-111111111111"
const actorUser = "22222222-2222-4222-8222-222222222222"
const actorOtherUser = "33333333-3333-4333-8333-333333333333"

type projectedAuth struct {
	actor                   httpapi.Actor
	authErr, resolveErr     error
	result                  context.Context
	nilContext              bool
	authCalls, resolveCalls int
}

func (a *projectedAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	a.authCalls++
	if a.authErr != nil || a.nilContext {
		return nil, a.authErr
	}
	if a.result != nil {
		return a.result, nil
	}
	return context.WithValue(ctx, struct{ authenticated bool }{true}, true), nil
}
func (a *projectedAuth) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	a.resolveCalls++
	if ctx == nil || spec.OperationID != "actorCommand" || r.PathValue("tenant_id") != actorTenant {
		return httpapi.Actor{}, errors.New("resolver missing successful route admission")
	}
	return a.actor, a.resolveErr
}

func actorRouter(t *testing.T, auth httpapi.Authenticator, access httpapi.Access, handler http.HandlerFunc) *httpapi.Router {
	t.Helper()
	router := httpapi.New(auth, nil)
	spec := httpapi.RouteSpec{OperationID: "actorCommand", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/products", Access: access}
	if access != httpapi.Public {
		spec.Scope = "returns:write"
	}
	if err := router.Register(spec, handler); err != nil {
		t.Fatal(err)
	}
	return router
}
func actorStatus(t *testing.T, router *httpapi.Router, ctx context.Context) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/t/"+actorTenant+"/products", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	// Even plausible caller-supplied principal fields are never admission data.
	req.Header.Set("X-Actor-ID", actorOtherUser)
	req.Header.Set("X-Actor-Kind", "service")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}
func requireActor(w http.ResponseWriter, r *http.Request) {
	if _, err := httpapi.AuthorizedActor(r.Context()); err != nil {
		httpapi.WriteProblem(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func TestAuthorizedActorPrivateProjection(t *testing.T) {
	for _, kind := range []string{"user", "api_key", "service"} {
		t.Run(kind, func(t *testing.T) {
			auth := &projectedAuth{actor: httpapi.Actor{Kind: kind, ID: actorUser}}
			router := actorRouter(t, auth, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, r *http.Request) {
				actor, err := httpapi.AuthorizedActor(r.Context())
				if err != nil {
					httpapi.WriteProblem(w, r, err)
					return
				}
				if actor != auth.actor {
					t.Errorf("actor=%+v want %+v", actor, auth.actor)
				}
				if tenant, err := httpapi.AuthorizedTenant(r.Context()); err != nil || tenant != actorTenant {
					t.Errorf("tenant admission lost: %q %v", tenant, err)
				}
				w.WriteHeader(204)
			})
			if got := actorStatus(t, router, nil); got != 204 || auth.authCalls != 1 || auth.resolveCalls != 1 {
				t.Fatalf("status=%d authenticate=%d resolve=%d", got, auth.authCalls, auth.resolveCalls)
			}
		})
	}
}

func TestAuthorizedActorMissingAndMalformed(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background(), context.WithValue(context.Background(), "actor", httpapi.Actor{Kind: "user", ID: actorUser})} {
		if _, err := httpapi.AuthorizedActor(ctx); err == nil {
			t.Fatal("unstamped actor accepted")
		}
	}
	for _, actor := range []httpapi.Actor{
		{}, {Kind: "customer", ID: actorUser}, {Kind: " user", ID: actorUser},
		{Kind: "user"}, {Kind: "user", ID: "malformed"},
		{Kind: "user", ID: "00000000-0000-0000-0000-000000000000"},
		{Kind: "user", ID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"},
		{Kind: "user", ID: "22222222222242228222222222222222"},
	} {
		auth := &projectedAuth{actor: actor}
		if got := actorStatus(t, actorRouter(t, auth, httpapi.AdminOrAPIKey, requireActor), nil); got != 401 {
			t.Errorf("malformed actor %+v status=%d want401", actor, got)
		}
	}
	// Not implementing the resolver remains compatible with existing handlers.
	legacy := actorRouter(t, acceptTenant{}, httpapi.AdminOrAPIKey, requireActor)
	if got := actorStatus(t, legacy, nil); got != 401 {
		t.Fatalf("legacy actor-requiring command status=%d", got)
	}
	legacy = actorRouter(t, acceptTenant{}, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	if got := actorStatus(t, legacy, nil); got != 204 {
		t.Fatalf("legacy actor-optional command status=%d", got)
	}
	// A composed authenticator can expose the optional resolver while the
	// selected route uses a legacy/cart authenticator with no actor projection.
	absent := &projectedAuth{}
	legacy = actorRouter(t, absent, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	if got := actorStatus(t, legacy, nil); got != 204 {
		t.Fatalf("actor-optional wrapper with absent projection status=%d", got)
	}
}

func TestAuthorizedActorResolvedOnlyAfterAuthentication(t *testing.T) {
	for _, auth := range []*projectedAuth{
		{actor: httpapi.Actor{Kind: "user", ID: actorUser}, authErr: apperrors.New(403, "forbidden", "denied")},
		{actor: httpapi.Actor{Kind: "user", ID: actorUser}, nilContext: true},
	} {
		calls := 0
		router := actorRouter(t, auth, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(204) })
		if got := actorStatus(t, router, nil); got < 400 || auth.resolveCalls != 0 || calls != 0 {
			t.Fatalf("failed authentication status=%d resolve=%d handler=%d", got, auth.resolveCalls, calls)
		}
	}
	auth := &projectedAuth{actor: httpapi.Actor{Kind: "user", ID: actorUser}, resolveErr: apperrors.New(401, "unauthenticated", "actor missing")}
	if got := actorStatus(t, actorRouter(t, auth, httpapi.AdminOrAPIKey, requireActor), nil); got != 401 {
		t.Fatalf("resolver error status=%d", got)
	}
}

func TestAuthorizedActorPublicAndCrossActorClear(t *testing.T) {
	var stamped context.Context
	first := &projectedAuth{actor: httpapi.Actor{Kind: "user", ID: actorUser}}
	router := actorRouter(t, first, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, r *http.Request) {
		stamped = r.Context()
		requireActor(w, r)
	})
	if got := actorStatus(t, router, nil); got != 204 {
		t.Fatalf("initial actor status=%d", got)
	}
	publicAuth := &projectedAuth{actor: first.actor}
	public := actorRouter(t, publicAuth, httpapi.Public, requireActor)
	if got := actorStatus(t, public, stamped); got != 401 || publicAuth.authCalls != 0 || publicAuth.resolveCalls != 0 {
		t.Fatalf("public reused actor: status=%d auth=%d resolver=%d", got, publicAuth.authCalls, publicAuth.resolveCalls)
	}
	legacy := actorRouter(t, authFunc(func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error) { return stamped, nil }), httpapi.AdminOrAPIKey, requireActor)
	if got := actorStatus(t, legacy, stamped); got != 401 {
		t.Fatalf("authenticator returned stale actor: status=%d", got)
	}
	second := &projectedAuth{actor: httpapi.Actor{Kind: "user", ID: actorOtherUser}, result: stamped}
	router = actorRouter(t, second, httpapi.AdminOrAPIKey, func(w http.ResponseWriter, r *http.Request) {
		actor, err := httpapi.AuthorizedActor(r.Context())
		if err != nil || actor != second.actor {
			t.Errorf("cross actor stamp=%+v %v", actor, err)
		}
		w.WriteHeader(204)
	})
	if got := actorStatus(t, router, stamped); got != 204 {
		t.Fatalf("replacement actor status=%d", got)
	}
}
