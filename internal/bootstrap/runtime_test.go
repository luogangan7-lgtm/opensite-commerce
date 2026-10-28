// Responsibilities: verify no-config composition remains safe and all optional
// runtime ports stay absent. Dependencies: bootstrap environment wiring; Go
// gates call this test, no real database/authentication is constructed.
package bootstrap_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoDatabaseRuntimeDefaultsDeny(t *testing.T) {
	t.Setenv("OPENSITE_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_ROOT", "")
	t.Setenv("OPENSITE_IDEMPOTENCY_ROOT", "")
	config, closeRuntime, err := bootstrap.FromEnvironment(context.Background(), bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime()
	if config.Catalog.Run != nil || config.Media.Run != nil || config.Fulfillment.Run != nil || config.Returns.Run != nil || config.Returns.Service != nil || config.Authenticator != nil {
		t.Fatal("unconfigured ports were fabricated")
	}
	server, err := bootstrap.New(config)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/t/11111111-1111-1111-1111-111111111111/products", nil))
	if rr.Code != 401 {
		t.Fatalf("default status=%d", rr.Code)
	}
}

// A composed authenticator must forward the already admitted stable actor.
// No bootstrap path derives it from a cookie/session or arbitrary client header.
type runtimeActorAuth struct{}

func (runtimeActorAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func (runtimeActorAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return httpapi.Actor{Kind: "user", ID: "22222222-2222-4222-8222-222222222222"}, nil
}

type runtimeLegacyAuth struct{}

func (runtimeLegacyAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func TestRuntimeActorForwardingAndOptionalAbsence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auth    httpapi.Authenticator
		require bool
		want    int
	}{
		{"projected", runtimeActorAuth{}, true, 204}, {"missing actor", runtimeLegacyAuth{}, true, 401}, {"legacy optional", runtimeLegacyAuth{}, false, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "actorProbe", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/actor-probe", Access: httpapi.AdminOrAPIKey, Scope: "probe:read"}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actor, err := httpapi.AuthorizedActor(r.Context())
				if tc.require && err != nil {
					httpapi.WriteProblem(w, r, err)
					return
				}
				if tc.require && (actor.Kind != "user" || actor.ID != "22222222-2222-4222-8222-222222222222") {
					t.Errorf("wrong actor %+v", actor)
				}
				w.WriteHeader(204)
			})}
			server, err := bootstrap.NewTestServer(t, bootstrap.Config{Authenticator: tc.auth, Routes: []httpapi.Route{route}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/t/11111111-1111-4111-8111-111111111111/actor-probe", nil))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
}

func TestRuntimeDurableCommandResponseRoot(t *testing.T) {
	t.Setenv("OPENSITE_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_ROOT", "")
	t.Setenv("OPENSITE_IDEMPOTENCY_ROOT", t.TempDir())
	config, closeRuntime, err := bootstrap.FromEnvironment(context.Background(), bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime()
	if config.Fulfillment.Idempotency.Store == nil || config.Fulfillment.Idempotency.Store != config.Returns.Idempotency.Store {
		t.Fatal("commands must share durable response store")
	}
	if config.Fulfillment.Run != nil || config.Returns.Run != nil || config.Returns.Service != nil || config.Authenticator != nil {
		t.Fatal("response root must not fabricate database, payment or auth ports")
	}
}

// Shipped compose sets a media root but neither OPENSITE_RESPONSE_ROOT nor a
// privacy DSN; CSV import replay must still have a store (thread T1).
func TestMediaRootResponseStoreWithoutPrivacy(t *testing.T) {
	t.Setenv("OPENSITE_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_DATABASE_URL", "")
	t.Setenv("OPENSITE_RESPONSE_ROOT", "")
	t.Setenv("OPENSITE_PRIVACY_DATABASE_URL", "")
	t.Setenv("OPENSITE_MEDIA_ROOT", t.TempDir())
	config, closeRuntime, err := bootstrap.FromEnvironment(context.Background(), bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime()
	if config.CatalogCSV.Responses == nil || config.Privacy.Responses == nil {
		t.Fatal("CSV/privacy replay store missing without privacy configuration")
	}
}
