// Notification transport gates exercise bootstrap's authenticated generated
// routes against a fake public port; they require no database or mail sender.
package notificationhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const id = "22222222-2222-4222-8222-222222222222"
const event = "33333333-3333-4333-8333-333333333333"
const base = "/api/v1/t/" + tenant + "/notifications"

type service struct {
	calls    int
	limit    int32
	key      string
	failure  error
	unsafe   bool
	empty    bool
	template string
}

func (s *service) ListFailures(ctx context.Context, t string, l int32) ([]contract.Failure, error) {
	s.calls++
	s.limit = l
	authorized, err := httpapi.AuthorizedTenant(ctx)
	if err != nil || authorized != t || t != tenant {
		return nil, apperrors.New(404, "not_found", "Notification not found")
	}
	if s.empty {
		return nil, s.failure
	}
	code := "TRANSPORT_ERROR"
	if s.unsafe {
		code = "customer@example.com"
	}
	key := s.template
	if key == "" {
		key = "order.paid"
	}
	return []contract.Failure{{ID: id, EventID: event, TemplateKey: key, Status: "uncertain", Attempts: 2, Generation: 1, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(101, 0).UTC(), SafeError: code}}, s.failure
}

func TestQuoteConvertedFailureRemainsInspectable(t *testing.T) {
	s := &service{template: "quote.converted"}
	w := request(t, s, authorized(t), "GET", base+"/failures", "", "Bearer notifications:write", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"template_key":"quote.converted"`) || strings.Contains(w.Body.String(), "@") {
		t.Fatalf("QUOTE_CONVERSION_NOTIFICATION_FAILURE_CONTRACT: %d %s", w.Code, w.Body)
	}
}
func (s *service) Redrive(ctx context.Context, t, n, k string) (contract.RedriveReceipt, error) {
	s.calls++
	s.key = k
	if t != tenant || n != id {
		return contract.RedriveReceipt{}, apperrors.New(404, "not_found", "Notification not found")
	}
	return contract.RedriveReceipt{NotificationID: n, Token: event, Status: "pending", Generation: 2}, s.failure
}

type authFunc func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error)

func (f authFunc) Authenticate(c context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	return f(c, r, s)
}
func authorized(t *testing.T) authFunc {
	return func(c context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
		if s.Access != httpapi.AdminOrAPIKey || s.Scope != "notifications:write" {
			t.Errorf("authorization contract: %+v", s)
		}
		if r.Header.Get("Authorization") == "" {
			return c, apperrors.New(401, "unauthenticated", "Credential required")
		}
		if r.Header.Get("Authorization") != "Bearer notifications:write" {
			return c, apperrors.New(403, "forbidden", "Scope required")
		}
		return c, nil
	}
}
func request(t *testing.T, s contract.Service, a httpapi.Authenticator, method, path, key, credential, body string) *httptest.ResponseRecorder {
	t.Helper()
	server, err := bootstrap.New(bootstrap.Config{Notifications: s, Authenticator: a})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if credential != "" {
		req.Header.Set("Authorization", credential)
	}
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)
	return w
}
func TestNotificationAdmissionAndAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, key, auth, body string
		status                              int
	}{
		{"default deny", "GET", base + "/failures", "", "", "", 401},
		{"scope deny", "GET", base + "/failures", "", "Bearer catalog:write", "", 403},
		{"invalid tenant", "GET", "/api/v1/t/bad/notifications/failures", "", "Bearer notifications:write", "", 400},
		{"invalid id", "POST", base + "/bad/redrive", "8372771c-f49f-582e-80b0-3e36a37d360f", "Bearer notifications:write", "", 400},
		{"missing key", "POST", base + "/" + id + "/redrive", "", "Bearer notifications:write", "", 400},
		{"space key", "POST", base + "/" + id + "/redrive", "bad key", "Bearer notifications:write", "", 400},
		{"unicode key", "POST", base + "/" + id + "/redrive", "é", "Bearer notifications:write", "", 400},
		{"long key", "POST", base + "/" + id + "/redrive", strings.Repeat("k", 129), "Bearer notifications:write", "", 400},
		{"zero limit", "GET", base + "/failures?limit=0", "", "Bearer notifications:write", "", 400},
		{"large limit", "GET", base + "/failures?limit=101", "", "Bearer notifications:write", "", 400},
		{"fractional limit", "GET", base + "/failures?limit=1.5", "", "Bearer notifications:write", "", 400},
		{"duplicate limit", "GET", base + "/failures?limit=1&limit=2", "", "Bearer notifications:write", "", 400},
		{"body forbidden", "POST", base + "/" + id + "/redrive", "8372771c-f49f-582e-80b0-3e36a37d360f", "Bearer notifications:write", "{}", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &service{}
			w := request(t, s, authorized(t), tc.method, tc.path, tc.key, tc.auth, tc.body)
			if w.Code != tc.status || s.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, s.calls, w.Body)
			}
		})
	}
	for _, method := range []string{"GET", "POST"} {
		path := base + "/failures"
		if method == "POST" {
			path = base + "/" + id + "/redrive"
		}
		w := request(t, nil, nil, method, path, "8372771c-f49f-582e-80b0-3e36a37d360f", "Bearer notifications:write", "")
		if w.Code != 401 {
			t.Fatalf("default=%d", w.Code)
		}
	}
}
func TestNotificationSafeListAndReplay(t *testing.T) {
	s := &service{}
	w := request(t, s, authorized(t), "GET", base+"/failures", "", "Bearer notifications:write", "")
	if w.Code != 200 || s.limit != 25 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list: %d %s limit %d", w.Code, w.Body, s.limit)
	}
	for _, private := range []string{"recipient", "contact", "subject", "body", "@"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("private field %s leaked: %s", private, w.Body)
		}
	}
	first := request(t, s, authorized(t), "POST", base+"/"+id+"/redrive", "909795ca-7a7c-5ed4-ac42-e3a70d7adaf2", "Bearer notifications:write", "")
	second := request(t, s, authorized(t), "POST", base+"/"+id+"/redrive", "909795ca-7a7c-5ed4-ac42-e3a70d7adaf2", "Bearer notifications:write", "")
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() || s.key != "909795ca-7a7c-5ed4-ac42-e3a70d7adaf2" || second.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("replay: %d %d %s %s", first.Code, second.Code, first.Body, second.Body)
	}
	s.unsafe = true
	w = request(t, s, authorized(t), "GET", base+"/failures", "", "Bearer notifications:write", "")
	if w.Code != 500 || strings.Contains(w.Body.String(), "@") {
		t.Fatalf("unsafe output: %d %s", w.Code, w.Body)
	}
}
func TestNotificationServiceErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{apperrors.New(409, "notification_conflict", "Cannot redrive uncertain delivery"), 409}, {apperrors.New(404, "notification_not_found", "Notification not found"), 404}, {errors.New("private@example.com SMTP body"), 500}} {
		s := &service{failure: tc.err}
		for _, method := range []string{"GET", "POST"} {
			path := base + "/failures"
			if method == "POST" {
				path = base + "/" + id + "/redrive"
			}
			w := request(t, s, authorized(t), method, path, "8372771c-f49f-582e-80b0-3e36a37d360f", "Bearer notifications:write", "")
			if w.Code != tc.code || strings.Contains(w.Body.String(), "@") {
				t.Fatalf("%s error: %d %s", method, w.Code, w.Body)
			}
		}
	}
	w := request(t, nil, authorized(t), "GET", base+"/failures", "", "Bearer notifications:write", "")
	if w.Code != 503 {
		t.Fatalf("nil service: %d %s", w.Code, w.Body)
	}
	other := "44444444-4444-4444-8444-444444444444"
	w = request(t, &service{}, authorized(t), "POST", "/api/v1/t/"+other+"/notifications/"+id+"/redrive", "8372771c-f49f-582e-80b0-3e36a37d360f", "Bearer notifications:write", "")
	if w.Code != 404 {
		t.Fatalf("cross tenant: %d %s", w.Code, w.Body)
	}
}

func TestNotificationEmptyListAndDuplicateKey(t *testing.T) {
	s := &service{empty: true}
	w := request(t, s, authorized(t), "GET", base+"/failures?limit=100", "", "Bearer notifications:write", "")
	if w.Code != 200 || s.limit != 100 || strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty list: %d %s limit %d", w.Code, w.Body, s.limit)
	}
	server, err := bootstrap.New(bootstrap.Config{Notifications: s, Authenticator: authorized(t)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", base+"/"+id+"/redrive", nil)
	req.Header.Set("Authorization", "Bearer notifications:write")
	req.Header.Add("Idempotency-Key", "b47bb372-4ddf-5f46-bdb1-f20e176b3941")
	req.Header.Add("Idempotency-Key", "dfd716d5-b8b1-5fb3-ad15-e56dd758fbf7")
	before := s.calls
	w = httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)
	if w.Code != 400 || s.calls != before {
		t.Fatalf("duplicate key: %d calls %d %s", w.Code, s.calls, w.Body)
	}
}
