// Responsibilities: verify the trusted tenant stamp comes only from private routing.
// Dependencies: platform/httpapi; callers: Go security gates; no business tables.
package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type acceptTenant struct{}

func (acceptTenant) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}
func TestAuthorizedTenantStamp(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	if _, err := httpapi.AuthorizedTenant(context.Background()); err == nil {
		t.Fatal("untrusted context accepted")
	}
	for _, tc := range []struct {
		access httpapi.Access
		tenant string
		want   int
	}{
		{httpapi.Public, id, 401}, {httpapi.AdminOrAPIKey, id, 204},
		{httpapi.AdminOrAPIKey, "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", 400},
		{httpapi.AdminOrAPIKey, "00000000-0000-0000-0000-000000000000", 400},
	} {
		router := httpapi.New(acceptTenant{}, nil)
		spec := httpapi.RouteSpec{OperationID: "stamp", Method: "GET", Pattern: "/t/{tenant_id}", Access: tc.access}
		if tc.access != httpapi.Public {
			spec.Scope = "catalog:read"
		}
		err := router.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, err := httpapi.AuthorizedTenant(r.Context())
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			if tenant != id {
				t.Errorf("stamp=%q", tenant)
			}
			w.WriteHeader(204)
		}))
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest("GET", "/t/"+tc.tenant, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s: %d want %d", tc.access, tc.tenant, rr.Code, tc.want)
		}
	}
}
