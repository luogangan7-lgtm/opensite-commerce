// Responsibilities: verify CSV admission limits and scope before persistence.
// Dependencies: public port/router; real PG gates prove atomic writes separately.
package catalogcsvhttp_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/catalogcsvhttp"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tenant = "11111111-1111-4111-8111-111111111111"

type auth struct{ scope string }

func (a auth) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	if s.Access != httpapi.AdminOrAPIKey || a.scope != s.Scope {
		return nil, apperrors.New(403, "forbidden", "Scope denied")
	}
	return ctx, nil
}

type spy struct {
	calls int
	contract.CSVPort
}

func (s *spy) Preflight(context.Context, pgx.Tx, io.Reader) (contract.CSVReport, error) {
	s.calls++
	return contract.CSVReport{Rows: 1, Errors: []contract.CSVRowError{{Row: 2, Field: "locale", Code: "required", Message: "Locale is required"}}}, nil
}
func TestCSVAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, route, scope, body, content, key string
		subject                                bool
		want, calls                            int
	}{
		{name: "wrong scope", route: "preflight", scope: "catalog:export", body: "csv", content: "application/octet-stream", subject: true, want: 403},
		{name: "missing subject", route: "preflight", scope: "catalog:import", body: "csv", content: "application/octet-stream", want: 401},
		{name: "wrong content", route: "preflight", scope: "catalog:import", body: "csv", content: "text/plain", subject: true, want: 415},
		{name: "size limit", route: "preflight", scope: "catalog:import", body: strings.Repeat("a", (1<<20)+1), content: "application/octet-stream", subject: true, want: 413},
		{name: "import key required", route: "import", scope: "catalog:import", body: "csv", content: "application/octet-stream", subject: true, want: 400},
		{name: "preflight diagnostics", route: "preflight", scope: "catalog:import", body: "csv", content: "application/octet-stream", subject: true, want: 200, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &spy{}
			cfg := catalogcsvhttp.Config{Service: svc, SubjectCommandConfig: httpapi.SubjectCommandConfig{Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }}}
			if tc.subject {
				cfg.Subject = func(context.Context) (httpapi.Subject, error) {
					return httpapi.Subject{Kind: "api_key", ID: tenant}, nil
				}
			}
			router := httpapi.New(auth{scope: tc.scope}, nil)
			for _, route := range catalogcsvhttp.New(cfg).Routes() {
				if err := router.Register(route.Spec, route.Handler); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest("POST", "/api/v1/t/"+tenant+"/catalog/csv/"+tc.route, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.content)
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want || svc.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", rr.Code, svc.calls, rr.Body.String())
			}
			if tc.calls == 1 && !strings.Contains(rr.Body.String(), `"row":2`) {
				t.Fatal("row diagnostics lost")
			}
		})
	}
}

type readingPort struct{ contract.CSVPort }

func (readingPort) Preflight(_ context.Context, _ pgx.Tx, input io.Reader) (contract.CSVReport, error) {
	_, err := io.ReadAll(input)
	return contract.CSVReport{}, err
}

func TestCSVUnknownLengthRequestTooLarge(t *testing.T) {
	for _, operation := range []string{"preflight", "import"} {
		t.Run(operation, func(t *testing.T) {
			cfg := catalogcsvhttp.Config{Service: readingPort{}, SubjectCommandConfig: httpapi.SubjectCommandConfig{
				Subject: func(context.Context) (httpapi.Subject, error) {
					return httpapi.Subject{Kind: "api_key", ID: tenant}, nil
				},
				Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) },
			}}
			router := httpapi.New(auth{scope: "catalog:import"}, nil)
			for _, route := range catalogcsvhttp.New(cfg).Routes() {
				if err := router.Register(route.Spec, route.Handler); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest("POST", "/api/v1/t/"+tenant+"/catalog/csv/"+operation, strings.NewReader(strings.Repeat("x", contract.CSVMaxBytes+1)))
			req.ContentLength = -1
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Idempotency-Key", "csv-size-test")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != 413 || !strings.Contains(rr.Body.String(), `"code":"request_too_large"`) {
				t.Fatalf("%s status=%d body=%s", operation, rr.Code, rr.Body.String())
			}
		})
	}
}
