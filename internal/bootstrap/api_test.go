// Bootstrap contract tests use httptest only. They guard startup refusal and
// operational health without creating pools, business routes or listeners.
package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestStartupRejectsInvalidRoute(t *testing.T) {
	s, err := bootstrap.New(bootstrap.Config{Routes: []httpapi.Route{{Spec: httpapi.RouteSpec{OperationID: "private", Method: "GET", Pattern: "/t/{tenant_id}/items", Access: httpapi.APIKey}, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid handler reached") })}}})
	if err == nil || s != nil {
		t.Fatal("invalid route did not stop server construction")
	}
}

func TestHealthAndNotFound(t *testing.T) {
	s, err := bootstrap.New(bootstrap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/healthz", 204}, {"/missing", 404}} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d", w.Code)
			}
			if tc.status == 404 && w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatal("404 must be a problem")
			}
		})
	}
}
