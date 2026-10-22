// Bootstrap contract tests use httptest only. They guard startup refusal and
// operational telemetry and request deadlines without pools or external listeners.
package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type deadlineAuth struct{ t *testing.T }

func (a deadlineAuth) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 75*time.Second {
		a.t.Fatal("authentication has no bounded request context")
	}
	return ctx, nil
}

func TestRequestDeadlineIncludesAuthenticationAndHandler(t *testing.T) {
	s, err := bootstrap.New(bootstrap.Config{Authenticator: deadlineAuth{t}, Routes: []httpapi.Route{{Spec: httpapi.RouteSpec{OperationID: "deadlineProbe", Method: "GET", Pattern: "/deadline", Access: httpapi.AdminSession}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 75*time.Second {
			t.Fatal("handler has no bounded request context")
		}
		w.WriteHeader(204)
	})}}})
	if err != nil {
		t.Fatal(err)
	}
	s.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/deadline", nil))
}

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
	for _, tc := range []struct{ method, path string }{{"POST", "/api/v1/t/" + id + "/quotes/" + id + "/orders"}, {"GET", "/api/v1/t/" + id + "/quotes/" + id + "/order-conversion"}, {"POST", "/api/public/v1/inquiries"}, {"POST", "/api/v1/t/" + id + "/quotes"}, {"GET", "/api/v1/t/" + id + "/quotes/" + id}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/send"}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/withdraw"}, {"POST", "/api/v1/t/" + id + "/quotes/" + id + "/expire"}, {"GET", "/api/public/v1/quotes/" + id}, {"POST", "/api/public/v1/quotes/" + id + "/accept"}, {"POST", "/api/public/v1/quotes/" + id + "/approve"}, {"POST", "/api/public/v1/quotes/" + id + "/reject"}} {
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 401 || w.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%s %s=%d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestOperationalMetricsObserveRecoveredStatus(t *testing.T) {
	s, err := bootstrap.New(bootstrap.Config{Routes: []httpapi.Route{{Spec: httpapi.RouteSpec{OperationID: "metricsProbePanic", Method: "GET", Pattern: "/probe-panic", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte("partial-secret"))
		panic("panic-secret")
	})}}})
	if err != nil {
		t.Fatal(err)
	}
	router, ok := s.Handler.(interface{ RouteSpecs() []httpapi.RouteSpec })
	if !ok {
		t.Fatalf("handler must expose registered RouteSpecs, got %T", s.Handler)
	}
	for _, spec := range router.RouteSpecs() {
		if spec.Pattern == "/metrics" {
			t.Fatal("operational metrics entered business route registry")
		}
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/healthz", 204}, {"/probe-panic", 500}, {"/customer-secret?token=query-secret", 404}} {
		w := httptest.NewRecorder()
		s.Handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s status=%d", tc.path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics status=%d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`opensite_http_requests_total{method="GET",status="204"} 1`, `opensite_http_requests_total{method="GET",status="500"} 1`, `opensite_http_errors_total{method="GET",status="500"} 1`, `opensite_http_request_duration_seconds_count{method="GET"} 3`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	for _, secret := range []string{"customer-secret", "query-secret", "panic-secret", "partial-secret", "probe-panic", "healthz"} {
		if strings.Contains(body, secret) {
			t.Errorf("metrics leaked %s", secret)
		}
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("content type=%q", w.Header().Get("Content-Type"))
	}
}
