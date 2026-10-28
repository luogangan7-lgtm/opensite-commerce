// Responsibility: verify fixture contracts never alter production admission.
// Dependencies: Router and the fixture helper; caller: Foundation Go lanes.
// Invariant: declared key admission remains enforced and no fallback leaks.
package httpapitest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/httpapitest"
)

func TestFixtureContractKeepsProductionFailClosed(t *testing.T) {
	spec := httpapi.RouteSpec{OperationID: "fixture", Method: "POST", Pattern: "/fixture", Access: httpapi.Public}
	router := httpapi.New(nil, nil)
	called := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(204) })
	if router.Register(spec, handler) == nil {
		t.Fatal("production registration admitted undeclared fixture")
	}
	if router.RegisterWithContract(spec, handler, nil) == nil {
		t.Fatal("missing fixture contract admitted")
	}
	if err := httpapitest.Register(t, router, true, spec, handler); err != nil {
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
