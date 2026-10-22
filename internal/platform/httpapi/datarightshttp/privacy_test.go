// Responsibilities: exercise production strict privacy routes and trust guards.
// Dependencies: public service fixture + router; SQL is covered by real PG gates.
package datarightshttp_test

import (
	"bytes"
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/datarightshttp"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const customer = "22222222-2222-4222-8222-222222222222"
const requestID = "33333333-3333-4333-8333-333333333333"
const path = "/api/v1/t/" + tenant + "/customers/" + customer + "/data-exports"

type subjectKey struct{}
type auth struct{ scope string }

func (a auth) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	if r.PathValue("tenant_id") != tenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	if s.Access != httpapi.AdminOrAPIKey || s.Scope != a.scope {
		return nil, apperrors.New(403, "forbidden", "Denied")
	}
	return context.WithValue(ctx, subjectKey{}, httpapi.Subject{Kind: "api_key", ID: customer}), nil
}
func subject(ctx context.Context) (httpapi.Subject, error) {
	s, _ := ctx.Value(subjectKey{}).(httpapi.Subject)
	return s, nil
}

type service struct {
	calls int
	body  []byte
	contract.Service
}

func (s *service) Status(context.Context, pgx.Tx, string, string, string) (contract.Request, error) {
	s.calls++
	return contract.Request{ID: requestID, Status: "ready", DownloadToken: strings.Repeat("a", 43)}, nil
}
func (s *service) Download(context.Context, string, string, string, string) ([]byte, error) {
	s.calls++
	return s.body, nil
}
func makeRouter(t *testing.T, a httpapi.Authenticator, svc *service, resolver httpapi.SubjectResolver, logs io.Writer) *httpapi.Router {
	t.Helper()
	r := httpapi.New(a, slog.New(slog.NewTextHandler(logs, nil)))
	cfg := datarightshttp.Config{Service: svc, SubjectCommandConfig: httpapi.SubjectCommandConfig{Subject: resolver, Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error { return fn(ctx, nil) }}}
	for _, route := range datarightshttp.New(cfg).Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func TestPrivacyHTTPAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, method, uri, body, key string
		auth                         httpapi.Authenticator
		resolve                      httpapi.SubjectResolver
		want                         int
	}{
		{name: "default deny", method: "POST", uri: path, key: "key", want: 401},
		{name: "scope denied", method: "GET", uri: path + "/" + requestID, auth: auth{scope: "catalog:export"}, resolve: subject, want: 403},
		{name: "missing validated subject", method: "POST", uri: path, key: "key", auth: auth{scope: "customers:privacy"}, want: 401},
		{name: "wire actor denied", method: "POST", uri: path, body: `{"actor_id":"` + customer + `"}`, key: "key", auth: auth{scope: "customers:privacy"}, resolve: subject, want: 400},
		{name: "missing key", method: "POST", uri: path, auth: auth{scope: "customers:privacy"}, resolve: subject, want: 400},
		{name: "unconfigured enqueue", method: "POST", uri: path, key: "key", auth: auth{scope: "customers:privacy"}, resolve: subject, want: 503},
		{name: "foreign tenant", method: "GET", uri: strings.Replace(path, tenant, requestID, 1) + "/" + requestID, auth: auth{scope: "customers:privacy"}, resolve: subject, want: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &service{}
			router := makeRouter(t, tc.auth, svc, tc.resolve, io.Discard)
			req := httptest.NewRequest(tc.method, tc.uri, strings.NewReader(tc.body))
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			req.Header.Set("X-Actor-ID", customer)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want || svc.calls != 0 {
				t.Fatalf("status=%d calls=%d %s", rr.Code, svc.calls, rr.Body.String())
			}
		})
	}
}
func TestPrivacyHTTPReadOnlyStatusAndBoundedDownload(t *testing.T) {
	svc := &service{body: bytes.Repeat([]byte("z"), 2<<20)}
	var logs bytes.Buffer
	router := makeRouter(t, auth{scope: "customers:privacy"}, svc, subject, &logs)
	for i, uri := range []string{path + "/" + requestID, path + "/" + requestID + "/download?token=" + strings.Repeat("a", 43)} {
		method := "GET"
		if i == 1 {
			method = "POST"
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(method, uri, nil))
		if rr.Code != 200 {
			t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private response cacheable")
		}
	}
	if svc.calls != 2 || strings.Contains(logs.String(), strings.Repeat("a", 43)) {
		t.Fatal("token logged or read failed")
	}
	svc.body = bytes.Repeat([]byte("s"), (10<<20)+1)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("POST", path+"/"+requestID+"/download?token="+strings.Repeat("a", 43), nil))
	if rr.Code != 500 || rr.Body.Len() > 1024 || strings.Contains(rr.Body.String(), strings.Repeat("s", 100)) {
		t.Fatalf("overflow leaked: %d %d", rr.Code, rr.Body.Len())
	}
}

// Metadata probes must never consume the single-use capability (FIX-44).
func TestPrivacyHTTPHeadAndGetDoNotConsumeDownload(t *testing.T) {
	svc := &service{body: []byte("{}")}
	router := makeRouter(t, auth{scope: "customers:privacy"}, svc, subject, io.Discard)
	uri := path + "/" + requestID + "/download?token=" + strings.Repeat("a", 43)
	for _, method := range []string{"HEAD", "GET"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(method, uri, nil))
		if rr.Code != 405 || svc.calls != 0 {
			t.Fatalf("%s status=%d calls=%d", method, rr.Code, svc.calls)
		}
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("POST", uri, nil))
	if rr.Code != 200 || svc.calls != 1 {
		t.Fatalf("POST after probes status=%d calls=%d", rr.Code, svc.calls)
	}
}
