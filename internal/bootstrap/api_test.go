// Bootstrap contract tests use httptest only. They guard startup refusal and
// operational health without creating pools, business routes or listeners.
package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
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

func TestStartupRejectsInvalidMediaRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := bootstrap.New(bootstrap.Config{Media: media.Config{Root: filepath.Join(file, "media")}})
	if err == nil || s != nil {
		t.Fatal("invalid configured media root did not stop server construction")
	}
}

func TestQuotesUnconfiguredAuthenticationIsDenied(t *testing.T) {
	server, err := bootstrap.New(bootstrap.Config{Quotes: quotes.NewApplication(nil)})
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct{ method, path string }{{"POST", "/api/public/v1/inquiries"}, {"POST", "/api/v1/t/" + id + "/quotes"}, {"GET", "/api/v1/t/" + id + "/quotes/" + id}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/send"}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/withdraw"}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/expire"}, {"GET", "/api/public/v1/quotes/" + id}, {"POST", "/api/public/v1/quotes/" + id + "/accept"}, {"POST", "/api/public/v1/quotes/" + id + "/approve"}, {"POST", "/api/public/v1/quotes/" + id + "/reject"}} {
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 401 || w.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%s %s=%d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
