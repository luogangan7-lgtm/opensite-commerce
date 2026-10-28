// Responsibilities: test safe CMS composition and trusted actor propagation.
// Dependencies: bootstrap and Router admission; caller: offline/race gates.
// Invariant: no runtime configuration authorizes staff or fabricates identity.
package bootstrap_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"net/http"
	"net/http/httptest"
	"testing"
)

type cmsStaffAuth struct{}

func (cmsStaffAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (cmsStaffAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, nil
}
func TestCMSDefaultAdmissionAndActorComposition(t *testing.T) {
	server, err := bootstrap.New(bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/t/11111111-1111-4111-8111-111111111111/sites/22222222-2222-4222-8222-222222222222/drafts/33333333-3333-4333-8333-333333333333", "/api/v1/t/11111111-1111-4111-8111-111111111111/sites/22222222-2222-4222-8222-222222222222/releases"} {
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("default CMS admission=%d", w.Code)
		}
	}
	spec := httpapi.RouteSpec{OperationID: "actorCompositionProbe", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/actor-probe", Access: httpapi.AdminOrAPIKey, Scope: "cms:write"}
	route := httpapi.Route{Spec: spec, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := httpapi.AuthorizedActor(r.Context())
		if err != nil || actor.ID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Errorf("trusted actor lost: %v %v", actor, err)
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	})}
	server, err = bootstrap.NewTestServer(t, bootstrap.Config{Authenticator: cmsStaffAuth{}, Routes: []httpapi.Route{route}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/t/11111111-1111-4111-8111-111111111111/actor-probe", nil))
	if w.Code != 204 {
		t.Fatalf("actor composition=%d", w.Code)
	}
}
