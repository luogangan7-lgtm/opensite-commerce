// Responsibilities: price API admission through production bootstrap/router.
// Dependencies: identity role contract and HTTP routing; caller: CI Go tests.
// Invariant: only catalog writers reach persistence; all mutations use list CAS.
package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type pricingAuth string

func (role pricingAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	for _, scope := range contract.RoleScopes(string(role)) {
		if spec.Scope == scope && spec.Access == httpapi.AdminOrAPIKey {
			return ctx, nil
		}
	}
	return nil, apperrors.New(403, "forbidden", "Scope denied")
}

func TestStaffPricingAdmission(t *testing.T) {
	base := "/api/v1/t/11111111-1111-4111-8111-111111111111/price-lists"
	list := base + "/22222222-2222-4222-8222-222222222222"
	tier := list + "/variants/33333333-3333-4333-8333-333333333333/prices/1"
	body := `{"currency":"USD","status":"active","starts_at":"2026-01-01T00:00:00Z","priority":0,"audience":"all"}`
	role := pricingAuth("owner")
	server, err := bootstrap.New(bootstrap.Config{Authenticator: &role})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner", "admin", "catalog_editor", "viewer", "content_editor"} {
		role = pricingAuth(name)
		for _, tc := range []struct {
			method, path, body, version string
			want                        int
		}{
			{"GET", base, "", "", 503},
			{"GET", list, "", "", 503},
			{"POST", base, body, "", 503},
			{"PUT", list, body, `"1"`, 503},
			{"PUT", list, body, "", 428},
			{"PUT", tier, `{"unit_amount":12}`, `"1"`, 503},
			{"DELETE", tier, "", `"1"`, 503},
			{"PUT", tier, `{"unit_amount":-1}`, `"1"`, 400},
			{"PUT", tier, `{"unit_amount":9223372036854775808}`, `"1"`, 400},
			{"PUT", tier, `{"unit_amount":null}`, `"1"`, 400},
			{"PUT", tier, `{"unit_amount":1}`, `W/"1"`, 400},
		} {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.version != "" {
				r.Header.Set("If-Match", tc.version)
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, r)
			want := tc.want
			if role == "viewer" || role == "content_editor" {
				want = 403
			}
			if w.Code != want {
				t.Errorf("PRICING_ADMISSION %s %s %s: got %d want %d: %s", role, tc.method, tc.path, w.Code, want, w.Body.String())
			}
		}
	}
}
