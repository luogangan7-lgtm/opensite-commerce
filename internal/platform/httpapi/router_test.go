// HTTP contract tests exercise public registration and httptest requests.
// They depend only on the platform API and stdlib; CI runs them with race checks.
// Invalid metadata and absent authentication must never reach private handlers.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func validSpec() httpapi.RouteSpec {
	return httpapi.RouteSpec{OperationID: "listProducts", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/products", Access: httpapi.AdminSession, Scope: "catalog.read"}
}

func TestRegisterInvalid(t *testing.T) {
	tests := []struct {
		name   string
		change func(*httpapi.RouteSpec)
	}{
		{"unknown access", func(s *httpapi.RouteSpec) { s.Access = "unknown" }},
		{"empty access", func(s *httpapi.RouteSpec) { s.Access = "" }},
		{"empty operation", func(s *httpapi.RouteSpec) { s.OperationID = "" }},
		{"empty method", func(s *httpapi.RouteSpec) { s.Method = "" }},
		{"bad method", func(s *httpapi.RouteSpec) { s.Method = "GET /oops" }},
		{"bad pattern", func(s *httpapi.RouteSpec) { s.Pattern = "not/a/path" }},
		{"tenant without scope", func(s *httpapi.RouteSpec) { s.Scope = "" }},
		{"tenant whitespace scope", func(s *httpapi.RouteSpec) { s.Scope = " " }},
		{"self scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.AuthenticatedSelf; s.Pattern = "/auth/whoami" }},
		{"self tenant", func(s *httpapi.RouteSpec) { s.Access = httpapi.AuthenticatedSelf; s.Scope = "" }},
		{"self tenant alias", func(s *httpapi.RouteSpec) {
			s.Access = httpapi.AuthenticatedSelf
			s.Scope = ""
			s.Pattern = "/t/{tenant}/sessions"
		}},
		{"self tenant rest", func(s *httpapi.RouteSpec) {
			s.Access = httpapi.AuthenticatedSelf
			s.Scope = ""
			s.Pattern = "/t/{tenant_id...}"
		}},
		{"tenant camelCase no scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.APIKey; s.Scope = ""; s.Pattern = "/t/{tenantId}/a" }},
		{"tenant mixed case no scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.APIKey; s.Scope = ""; s.Pattern = "/t/{Tenant_ID}/a" }},
		{"tenant uuid no scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.APIKey; s.Scope = ""; s.Pattern = "/t/{tenant_uuid}/a" }},
		{"tenant alias no scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.AdminSession; s.Scope = ""; s.Pattern = "/t/{tenant}/a" }},
		{"self tenant upper", func(s *httpapi.RouteSpec) {
			s.Access = httpapi.AuthenticatedSelf
			s.Scope = ""
			s.Pattern = "/t/{TENANT}/a"
		}},
		{"tenant non-canonical with scope", func(s *httpapi.RouteSpec) { s.Pattern = "/t/{tenantId}/products" }},
		{"wildcard cyrillic lookalike", func(s *httpapi.RouteSpec) { s.Scope = ""; s.Pattern = "/t/{t\u0435nant_id}/a" }},
		{"wildcard fullwidth lookalike", func(s *httpapi.RouteSpec) { s.Scope = ""; s.Pattern = "/t/{\uff54enant_id}/a" }},
		{"wildcard uppercase name", func(s *httpapi.RouteSpec) { s.Pattern = "/t/{tenant_id}/{Product}" }},
		{"negative size", func(s *httpapi.RouteSpec) { s.MaxBodySize = -1 }},
		{"public scope", func(s *httpapi.RouteSpec) { s.Access = httpapi.Public }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.change(&s)
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("Register panicked instead of returning an error: %v", p)
				}
			}()
			if err := httpapi.New(nil, nil).Register(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
				t.Error("invalid route registered")
			}
		})
	}
	t.Run("nil handler", func(t *testing.T) {
		defer func() {
			if recover() != nil {
				t.Error("nil handler must return error")
			}
		}()
		if httpapi.New(nil, nil).Register(validSpec(), nil) == nil {
			t.Error("nil handler registered")
		}
	})
}

func TestRegisterAllowedAndConflicts(t *testing.T) {
	for _, access := range []httpapi.Access{httpapi.Public, httpapi.AuthenticatedSelf, httpapi.AdminSession, httpapi.APIKey, httpapi.AdminOrAPIKey, httpapi.InternalService} {
		t.Run(string(access), func(t *testing.T) {
			s := validSpec()
			s.Access = access
			if access == httpapi.Public || access == httpapi.AuthenticatedSelf {
				s.Scope = ""
				s.Pattern = "/auth/whoami"
			}
			if err := httpapi.New(nil, nil).Register(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err != nil {
				t.Fatal(err)
			}
		})
	}
	r := httpapi.New(nil, nil)
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	s := validSpec()
	if err := r.Register(s, h); err != nil {
		t.Fatal(err)
	}
	t.Run("duplicate operation", func(t *testing.T) {
		other := s
		other.Pattern = "/other"
		if r.Register(other, h) == nil {
			t.Error("duplicate operation registered")
		}
	})
	t.Run("mux conflict", func(t *testing.T) {
		other := s
		other.OperationID = "another"
		defer func() {
			if recover() != nil {
				t.Error("mux conflict must return error")
			}
		}()
		if r.Register(other, h) == nil {
			t.Error("duplicate pattern registered")
		}
	})
}

type authFunc func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error)

func (f authFunc) Authenticate(c context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	return f(c, r, s)
}

func TestDefaultDenyAndPublic(t *testing.T) {
	for _, access := range []httpapi.Access{httpapi.Public, httpapi.AuthenticatedSelf, httpapi.AdminSession, httpapi.APIKey, httpapi.AdminOrAPIKey, httpapi.InternalService} {
		t.Run(string(access), func(t *testing.T) {
			r := httpapi.New(nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
			s := httpapi.RouteSpec{OperationID: "whoami", Method: "GET", Pattern: "/auth/whoami", Access: access}
			called := false
			if err := r.Register(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/auth/whoami", nil))
			if access == httpapi.Public {
				if w.Code != 200 || !called {
					t.Fatal("public route unreachable")
				}
				return
			}
			if w.Code != 401 || called {
				t.Fatalf("private route reached: status=%d called=%v", w.Code, called)
			}
			assertProblem(t, w, 401)
		})
	}
}

func TestAuthenticatorReceivesResolvedRoute(t *testing.T) {
	called := false
	r := httpapi.New(authFunc(func(c context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
		if s.Scope != "catalog.read" || r.PathValue("tenant_id") != "tenant-a" {
			t.Error("auth did not receive resolved route")
		}
		called = true
		return c, nil
	}), nil)
	if err := r.Register(validSpec(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/t/tenant-a/products", nil))
	if !called || w.Code != 204 {
		t.Fatal("authentication seam did not run")
	}
}

func TestNilAuthenticationContextDenied(t *testing.T) {
	r := httpapi.New(authFunc(func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error) { return nil, nil }), nil)
	if err := r.Register(validSpec(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("handler reached") })); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/t/a/products", nil))
	assertProblem(t, w, 401)
}

func TestAuthenticationContextKeepsRequestID(t *testing.T) {
	r := httpapi.New(authFunc(func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error) {
		return context.Background(), nil
	}), nil)
	if err := r.Register(validSpec(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteProblem(w, r, apperrors.New(403, "", "denied"))
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/t/a/products", nil))
	assertProblem(t, w, 403)
	var p struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.RequestID == "" || p.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatal("auth context lost server request correlation")
	}
}

func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status/type=%d/%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var p struct {
		Type, Code string
		Status     int
	}
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Type != "about:blank" || p.Code == "" || p.Status != status {
		t.Fatal("invalid problem document", w.Body.String())
	}
}

func TestPanicRecoveryAndPrivateLogs(t *testing.T) {
	var logs bytes.Buffer
	r := httpapi.New(nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := r.Register(httpapi.RouteSpec{OperationID: "panicProbe", Method: "GET", Pattern: "/panic", Access: httpapi.Public}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "should-not-escape")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("partial-secret"))
		panic("panic-secret")
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panic?token=query-secret", nil)
	req.Header.Set("Authorization", "Bearer auth-secret")
	req.Header.Set("X-Request-ID", "untrusted-secret")
	defer func() {
		if recover() != nil {
			t.Error("panic escaped HTTP recovery")
		}
	}()
	r.ServeHTTP(w, req)
	assertProblem(t, w, 500)
	if w.Header().Get("Set-Cookie") != "" || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("partial panic response leaked")
	}
	id := w.Header().Get("X-Request-ID")
	if len(id) != 32 {
		t.Fatal("missing server request ID")
	}
	var p struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.RequestID != id {
		t.Fatal("problem and header request ID differ")
	}
	if !strings.Contains(logs.String(), id) || strings.Contains(logs.String(), "secret") {
		t.Fatal("log correlation or redaction failed", logs.String())
	}
}

func TestBufferedWriteSniffsContentType(t *testing.T) {
	r := httpapi.New(nil, nil)
	spec := httpapi.RouteSpec{OperationID: "sniff", Method: "GET", Pattern: "/sniff", Access: httpapi.Public}
	if err := r.Register(spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body>hi</body></html>"))
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/sniff", nil))
	if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want sniffed text/html", got)
	}
}
