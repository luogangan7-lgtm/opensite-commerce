// Responsibilities: exercise every declared idempotent HTTP ingress before any
// handler can persist caller text. Dependencies: embedded OAS and Router;
// callers: CI Go/pgx lanes. Invariant: rejected input cannot enter a writer.
package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type uuidBoundaryAuth struct{}

func (uuidBoundaryAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	return ctx, nil
}

func TestEveryIdempotencyEndpointRejectsPersonalKeyBeforeAnyHandler(t *testing.T) {
	doc, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for pattern, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			parameters := append(append(openapi3.Parameters{}, item.Parameters...), op.Parameters...)
			hasKey := false
			for _, parameter := range parameters {
				if parameter.Value != nil && parameter.Value.In == "header" && strings.EqualFold(parameter.Value.Name, "Idempotency-Key") {
					hasKey = true
				}
			}
			if !hasKey || op.Extensions["x-opensite-status"] != "implemented" {
				continue
			}
			count++
			t.Run(op.OperationID, func(t *testing.T) {
				access, _ := op.Extensions["x-opensite-access"].(string)
				scope, _ := op.Extensions["x-required-scope"].(string)
				spec := httpapi.RouteSpec{OperationID: op.OperationID, Method: method, Pattern: pattern, Access: httpapi.Access(access), Scope: scope}
				path := pattern
				for _, part := range strings.Split(pattern, "/") {
					if strings.HasPrefix(part, "{") {
						path = strings.ReplaceAll(path, part, "11111111-1111-4111-8111-111111111111")
					}
				}
				for _, key := range []string{"alice@example.test", "", "legacy-printable-key", "urn:uuid:abcdabcd-abcd-abcd-abcd-abcdabcdabcd", "abcdabcdabcdabcdabcdabcdabcdabcd", "{abcdabcd-abcd-abcd-abcd-abcdabcdabcd}"} {
					t.Run(key, func(t *testing.T) {
						persisted := 0
						router := httpapi.New(uuidBoundaryAuth{}, nil)
						if err := router.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							persisted++
							w.WriteHeader(204)
						})); err != nil {
							t.Fatal(err)
						}
						request := httptest.NewRequest(method, path, strings.NewReader(`{}`))
						request.Header.Set("Idempotency-Key", key)
						response := httptest.NewRecorder()
						router.ServeHTTP(response, request)
						if response.Code != 400 || !strings.Contains(response.Body.String(), `"code":"invalid_idempotency_key"`) || persisted != 0 {
							t.Fatalf("%s %s: status=%d persisted=%d body=%s", method, pattern, response.Code, persisted, response.Body.String())
						}
					})
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("no Idempotency-Key operations exercised")
	}
	t.Logf("covered %d idempotent operations; no rejected key reached a handler", count)
}

func TestEveryIdempotencyHeaderDeclaresUUID(t *testing.T) {
	doc, err := httpapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	for name, parameter := range doc.Components.Parameters {
		if parameter.Value != nil && parameter.Value.In == "header" && strings.EqualFold(parameter.Value.Name, "Idempotency-Key") {
			if parameter.Value.Schema == nil || parameter.Value.Schema.Value.Format != "uuid" {
				t.Errorf("%s: Idempotency-Key requires format uuid", name)
			}
		}
	}
}

func TestUUIDBoundaryNormalizesBeforeAnyHandler(t *testing.T) {
	const lower = "abcdefab-cdef-4bcd-8fab-cdefabcdefab"
	const pattern = "/api/v1/t/{tenant_id}/checkout"
	router := httpapi.New(uuidBoundaryAuth{}, nil)
	called := 0
	if err := router.Register(httpapi.RouteSpec{OperationID: "createCheckout", Method: "POST", Pattern: pattern, Access: httpapi.Storefront}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if got := r.Header.Get("Idempotency-Key"); got != lower {
			t.Errorf("handler received noncanonical key %q", got)
		}
		w.WriteHeader(204)
	})); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{strings.ToUpper(lower), lower} {
		request := httptest.NewRequest("POST", "/api/v1/t/11111111-1111-4111-8111-111111111111/checkout", nil)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 204 {
			t.Fatalf("canonical UUID rejected: %d %s", response.Code, response.Body.String())
		}
	}
	if called != 2 {
		t.Fatalf("handler calls=%d", called)
	}
}
