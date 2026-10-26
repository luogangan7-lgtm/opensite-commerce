// Responsibilities: test catalog admission through the production platform router.
// Dependencies: generated DTOs/platform routing; callers: focused Go gates.
// Invariants: unauthorized requests and malformed writes never touch persistence.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

const tenantA = "11111111-1111-4111-8111-111111111111"
const tenantB = "22222222-2222-4222-8222-222222222222"
const missingID = "99999999-9999-4999-8999-999999999999"

type testAuth struct {
	tenant string
	denied bool
}

func (a testAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.denied {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	if r.PathValue("tenant_id") != a.tenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	if spec.Access != httpapi.AdminOrAPIKey || (spec.Scope != "catalog:read" && spec.Scope != "catalog:write") {
		return nil, apperrors.New(403, "forbidden", "Invalid catalog access")
	}
	return ctx, nil
}
func testRouter(t *testing.T, s *Service, auth httpapi.Authenticator) *httpapi.Router {
	t.Helper()
	r := httpapi.New(auth, nil)
	for _, route := range s.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
// testResponse holds the response the client received, including frozen headers.
type testResponse struct {
	Code int
	Body *bytes.Buffer
	header http.Header
}

func (r *testResponse) Header() http.Header { return r.header }

func httpRequest(r http.Handler, method, path, body, version string) *testResponse {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if version != "" {
		req.Header.Set("If-Match", version)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	response := rr.Result()
	defer response.Body.Close()
	return &testResponse{Code: response.StatusCode, Body: rr.Body, header: response.Header}
}
func TestHTTPAdmission(t *testing.T) {
	product := "/api/v1/t/" + tenantA + "/products/" + missingID
	for _, tc := range []struct {
		name, method, path, body, version string
		auth                              httpapi.Authenticator
		want, calls                       int
	}{
		{name: "default deny", method: "GET", path: product, want: 401},
		{name: "scope denied", method: "POST", path: "/api/v1/t/" + tenantA + "/products", body: `{"product_code":"P"}`, auth: testAuth{tenant: tenantA, denied: true}, want: 403},
		{name: "foreign tenant", method: "GET", path: "/api/v1/t/" + tenantB + "/products/" + missingID, auth: testAuth{tenant: tenantA}, want: 404},
		{name: "required version", method: "PATCH", path: product, body: `{"product_code":"P"}`, auth: testAuth{tenant: tenantA}, want: 428},
		{name: "weak version", method: "DELETE", path: product, version: `W/"1"`, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "unknown field", method: "POST", path: "/api/v1/t/" + tenantA + "/products", body: `{"product_code":"P","price":1}`, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "duplicate key", method: "POST", path: "/api/v1/t/" + tenantA + "/products", body: `{"product_code":"P","product_code":"Q"}`, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "missing options", method: "POST", path: product + "/variants", body: `{"sku":"S"}`, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "null options", method: "POST", path: product + "/variants", body: `{"sku":"S","option_value_ids":null}`, auth: testAuth{tenant: tenantA}, want: 400},
		{name: "invalid cursor", method: "GET", path: "/api/v1/t/" + tenantA + "/products?cursor=!", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "negative page size", method: "GET", path: "/api/v1/t/" + tenantA + "/products?page_size=-1", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "excessive page size", method: "GET", path: "/api/v1/t/" + tenantA + "/products?page_size=101", auth: testAuth{tenant: tenantA}, want: 400},
		{name: "valid request reaches runner", method: "GET", path: product, auth: testAuth{tenant: tenantA}, want: 503, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := New(Config{Run: func(context.Context, string, func(*db.Queries) error) error {
				calls++
				return apperrors.New(503, "catalog_unavailable", "Test storage unavailable")
			}})
			r := testRouter(t, s, tc.auth)
			rr := httpRequest(r, tc.method, tc.path, tc.body, tc.version)
			if rr.Code != tc.want || calls != tc.calls {
				t.Fatalf("HTTP %d want %d, calls %d want %d: %s", rr.Code, tc.want, calls, tc.calls, rr.Body.String())
			}
			var problem map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil || problem["status"] != float64(tc.want) || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("problem mapping: %s", rr.Body.String())
			}
		})
	}
}
func TestDirectHandlerRejectsUntrustedContext(t *testing.T) {
	calls := 0
	s := New(Config{Run: func(context.Context, string, func(*db.Queries) error) error { calls++; return nil }})
	_, err := s.GetProduct(context.Background(), generated.GetProductRequestObject{TenantId: uuid.MustParse(tenantA), ProductId: uuid.MustParse(missingID)})
	domain, ok := err.(*apperrors.Error)
	if !ok || domain.Status != 401 || calls != 0 {
		t.Fatalf("direct request: %v calls %d", err, calls)
	}
}
