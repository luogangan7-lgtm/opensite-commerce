// Responsibility: verify fixture contracts never alter production admission.
// Dependencies: Router and the fixture helper; caller: Foundation Go lanes.
// Invariant: declared key admission remains enforced and no fallback leaks.
package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestFixtureContractKeepsProductionFailClosed(t *testing.T) {
	spec := httpapi.RouteSpec{OperationID: "fixture", Method: "POST", Pattern: "/fixture", Access: httpapi.Public}
	router := httpapi.New(nil, nil)
	called := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(204) })
	if router.Register(spec, handler) == nil {
		t.Fatal("production registration admitted undeclared fixture")
	}
	if httpapi.RegisterFixtureContract(router, spec, handler, nil) == nil {
		t.Fatal("missing fixture contract admitted")
	}
	op := &openapi3.Operation{OperationID: spec.OperationID, Parameters: openapi3.Parameters{{Value: &openapi3.Parameter{Name: "Idempotency-Key", In: "header", Required: true}}}}
	item := &openapi3.PathItem{}
	item.SetOperation(spec.Method, op)
	contract := &openapi3.T{Paths: openapi3.NewPaths()}
	contract.Paths.Set(spec.Pattern, item)
	if err := httpapi.RegisterFixtureContract(router, spec, handler, contract); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "alice@example.test", "abcdef01-2345-5678-abcd-ef0123456789"} {
		request := httptest.NewRequest("POST", "/fixture", nil)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		want := 400
		if len(key) == 36 {
			want = 204
		}
		if response.Code != want {
			t.Fatalf("fixture admission: got %d want %d", response.Code, want)
		}
	}
	if called != 1 || httpapi.New(nil, nil).Register(spec, handler) == nil {
		t.Fatal("fixture contract changed production registration or key admission")
	}
}
