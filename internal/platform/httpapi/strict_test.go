// Responsibilities: exercise real outer-router/generated-wrapper admission and
// route filtering. Dependencies: embedded OAS and strict service interface;
// security gates call these tests; assertions must fail on trust/JSON bypass.
package httpapi_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

type admissionService struct {
	generated.StrictServerInterface
	calls int
}

func (s *admissionService) CreateProduct(ctx context.Context, _ generated.CreateProductRequestObject) (generated.CreateProductResponseObject, error) {
	if _, err := httpapi.AuthorizedTenant(ctx); err != nil {
		return nil, err
	}
	s.calls++
	return nil, apperrors.New(503, "catalog_unavailable", "unconfigured")
}
func (s *admissionService) UpdateProduct(ctx context.Context, _ generated.UpdateProductRequestObject) (generated.UpdateProductResponseObject, error) {
	s.calls++
	return nil, apperrors.New(503, "catalog_unavailable", "unconfigured")
}
func (s *admissionService) ListProducts(ctx context.Context, _ generated.ListProductsRequestObject) (generated.ListProductsResponseObject, error) {
	s.calls++
	return nil, apperrors.New(503, "catalog_unavailable", "unconfigured")
}

func (s *admissionService) PutMediaUploadContent(ctx context.Context, r generated.PutMediaUploadContentRequestObject) (generated.PutMediaUploadContentResponseObject, error) {
	s.calls++
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, apperrors.New(413, "request_too_large", "too large")
	}
	return nil, apperrors.New(503, "media_unavailable", "unconfigured")
}
func admissionRouter(t *testing.T, auth httpapi.Authenticator) (*httpapi.Router, *admissionService) {
	t.Helper()
	s := &admissionService{}
	router := httpapi.New(auth, nil)
	base := "/api/v1/t/{tenant_id}/products"
	specs := []httpapi.RouteSpec{
		{OperationID: "putMediaUploadContent", Method: "PUT", Pattern: "/api/v1/t/{tenant_id}/media/uploads/{upload_id}/content", Access: httpapi.AdminOrAPIKey, Scope: "media:write", MaxBodySize: 10 << 20},
		{OperationID: "createProduct", Method: "POST", Pattern: base, Access: httpapi.AdminOrAPIKey, Scope: "catalog:write"},
		{OperationID: "updateProduct", Method: "PATCH", Pattern: base + "/{product_id}", Access: httpapi.AdminOrAPIKey, Scope: "catalog:write"},
		{OperationID: "listProducts", Method: "GET", Pattern: base, Access: httpapi.AdminOrAPIKey, Scope: "catalog:read"},
	}
	for _, route := range httpapi.StrictRoutes(s, specs) {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return router, s
}
func TestStrictRoutesAdmission(t *testing.T) {
	const base = "/api/v1/t/11111111-1111-1111-1111-111111111111/products"
	for _, tc := range []struct {
		name, method, path, body, match, content string
		want                                     int
		calls                                    int
	}{
		{"valid", "POST", base, `{"product_code":"p"}`, "", "application/json", 503, 1},
		{"unknown", "POST", base, `{"product_code":"p","surprise":true}`, "", "application/json", 400, 0},
		{"casefold", "POST", base, `{"PRODUCT_CODE":"p"}`, "", "application/json", 400, 0},
		{"duplicate", "POST", base, `{"product_code":"p","product_code":"q"}`, "", "application/json", 400, 0},
		{"trailing", "POST", base, `{"product_code":"p"} {}`, "", "application/json", 400, 0},
		{"media type", "POST", base, `{"product_code":"p"}`, "", "text/plain", 415, 0},
		{"missing version", "PATCH", base + "/22222222-2222-2222-2222-222222222222", `{"product_code":"p"}`, "", "application/json", 428, 0},
		{"malformed version", "PATCH", base + "/22222222-2222-2222-2222-222222222222", `{"product_code":"p"}`, "1", "application/json", 400, 0},
		{"weak version", "PATCH", base + "/22222222-2222-2222-2222-222222222222", `{"product_code":"p"}`, `W/"1"`, "application/json", 400, 0},
		{"version", "PATCH", base + "/22222222-2222-2222-2222-222222222222", `{"product_code":"p"}`, `"1"`, "application/json", 503, 1},
		{"query over limit", "GET", base + "?page_size=101", "", "", "", 400, 0},
		{"query duplicate", "GET", base + "?page_size=20&page_size=30", "", "", "", 400, 0},
		{"query valid", "GET", base + "?page_size=100", "", "", "", 503, 1},
		{"planned isolation", "GET", "/api/v1/auth/current-actor", "", "", "", 404, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, s := admissionRouter(t, acceptTenant{})
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.content)
			req.Header.Set("Idempotency-Key", "afe36e5c-6a6d-5f83-a817-48661fcdb1cc")
			if tc.match != "" {
				req.Header.Set("If-Match", tc.match)
			}
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want || s.calls != tc.calls {
				t.Fatalf("status=%d calls=%d want %d/%d body=%s", rr.Code, s.calls, tc.want, tc.calls, rr.Body.String())
			}
		})
	}
}
func TestStrictRoutesAuthenticationPrecedesAdmission(t *testing.T) {
	router, s := admissionRouter(t, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/t/not-a-uuid/products", strings.NewReader(`{"unknown":true}`)))
	if rr.Code != 401 || s.calls != 0 {
		t.Fatalf("%d calls %d", rr.Code, s.calls)
	}
}
func TestStrictRoutesRefusesPlannedOperation(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("planned operation accepted")
		}
	}()
	httpapi.StrictRoutes(&admissionService{}, []httpapi.RouteSpec{{OperationID: "getCurrentActor", Method: "GET", Pattern: "/api/v1/auth/current-actor", Access: httpapi.AuthenticatedSelf}})
}

func TestStrictHeaderAndBodyLimits(t *testing.T) {
	const base = "/api/v1/t/11111111-1111-1111-1111-111111111111/products"
	for _, tc := range []struct {
		name, method, body string
		keys, matches      []string
		want               int
	}{
		{"idempotency missing", "POST", `{"product_code":"p"}`, nil, nil, 400},
		{"idempotency empty", "POST", `{"product_code":"p"}`, []string{""}, nil, 400},
		{"idempotency oversized", "POST", `{"product_code":"p"}`, []string{strings.Repeat("x", 129)}, nil, 400},
		{"idempotency duplicate", "POST", `{"product_code":"p"}`, []string{"c4c408dc-7cf0-5f3b-bc23-e04632853ca6", "c57425b2-b1a6-5e74-96a3-5c9753cfac05"}, nil, 400},
		{"version duplicate", "PATCH", `{"product_code":"p"}`, []string{"c4c408dc-7cf0-5f3b-bc23-e04632853ca6"}, []string{`"1"`, `"2"`}, 400},
		{"json byte limit", "POST", `{"product_code":"` + strings.Repeat("x", 1<<20) + `"}`, []string{"c4c408dc-7cf0-5f3b-bc23-e04632853ca6"}, nil, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, s := admissionRouter(t, acceptTenant{})
			path := base
			if tc.method == "PATCH" {
				path += "/22222222-2222-2222-2222-222222222222"
			}
			req := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			for _, v := range tc.keys {
				req.Header.Add("Idempotency-Key", v)
			}
			for _, v := range tc.matches {
				req.Header.Add("If-Match", v)
			}
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want || s.calls != 0 {
				t.Fatalf("status=%d calls=%d want=%d", rr.Code, s.calls, tc.want)
			}
		})
	}
}

func TestStrictBinaryAdmission(t *testing.T) {
	const path = "/api/v1/t/11111111-1111-1111-1111-111111111111/media/uploads/22222222-2222-2222-2222-222222222222/content"
	for _, tc := range []struct {
		name, content, body string
		length              int64
		want, calls         int
	}{
		{"wrong type", "image/png", "x", 1, 415, 0},
		{"missing type", "", "x", 1, 415, 0},
		{"valid", "application/octet-stream", "x", 1, 503, 1},
		{"declared oversize", "application/octet-stream", "x", (10 << 20) + 1, 413, 0},
		{"streamed oversize", "application/octet-stream", strings.Repeat("x", (10<<20)+1), -1, 413, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, s := admissionRouter(t, acceptTenant{})
			req := httptest.NewRequest("PUT", path, strings.NewReader(tc.body))
			req.ContentLength = tc.length
			req.Header.Set("Content-Type", tc.content)
			req.Header.Set("Idempotency-Key", "708b6a73-5bd8-5ff9-8413-f5ce2471f983")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want || s.calls != tc.calls {
				t.Fatalf("status=%d calls=%d want %d/%d body=%s", rr.Code, s.calls, tc.want, tc.calls, rr.Body.String())
			}
		})
	}
}
