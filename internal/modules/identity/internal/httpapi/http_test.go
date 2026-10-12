// Transport regression tests exercise the actual platform router and login service.
// Repository doubles cover transport decisions; real SQL is verified by pgx tests.
// Cookie, CSRF and malformed input failures must occur before side effects.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type fakeRepo struct {
	saved, revoked, failures, credentials int
	credential                            login.Credential
	accountCooldown, pairCooldown         time.Time
	saveErr                               error
	lastIP                                netip.Addr
	session                               login.Session
	hash                                  [32]byte
	dead                                  bool
}

func (f *fakeRepo) Credential(context.Context, string) (login.Credential, error) {
	f.credentials++
	if f.credential.UserID != "" {
		return f.credential, nil
	}
	return login.Credential{}, login.ErrNotFound
}
func (f *fakeRepo) Cooldowns(_ context.Context, _ string, ip netip.Addr) (time.Time, time.Time, error) {
	f.lastIP = ip
	return f.accountCooldown, f.pairCooldown, nil
}
func (f *fakeRepo) RecordFailure(context.Context, string, netip.Addr, time.Time) error {
	f.failures++
	return nil
}
func (f *fakeRepo) Device(context.Context, [32]byte) (login.Device, error) {
	return login.Device{}, login.ErrNotFound
}
func (f *fakeRepo) FailDevice(context.Context, string, time.Time) error { return nil }
func (f *fakeRepo) SaveLogin(_ context.Context, in login.SessionInput) (login.Session, error) {
	f.saved++
	if in.OldTokenHash != nil {
		return f.session, f.saveErr
	}
	return f.session, nil
}
func (f *fakeRepo) LookupSession(_ context.Context, h [32]byte, _ time.Time, _ time.Duration) (login.Session, error) {
	if f.dead || h != f.hash {
		return login.Session{}, login.ErrSession
	}
	return f.session, nil
}
func (f *fakeRepo) RevokeSession(context.Context, string, time.Time) error {
	f.revoked++
	f.dead = true
	return nil
}
func transportFixture(t *testing.T) (*Handler, *platform.Router, *fakeRepo, *login.Service, string) {
	t.Helper()
	// Intentionally historical: browser MaxAge must follow service time, not wall time.
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := strings.Repeat("A", 43)
	f := &fakeRepo{session: login.Session{ID: "session", UserID: "user", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, hash: sha256.Sum256(make([]byte, 32))}
	s, e := login.New(f, login.Config{Origin: "https://admin.example.test", SigningKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	h := New(s, "https://admin.example.test", 30*time.Minute)
	r := platform.New(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range h.Routes() {
		if e := r.Register(route.Spec, route.Handler); e != nil {
			t.Fatal(e)
		}
	}
	return h, r, f, s, raw
}
func request(r *platform.Router, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	q := httptest.NewRequest(method, path, strings.NewReader(body))
	q.RemoteAddr = "192.0.2.1:54321"
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Origin", "https://admin.example.test")
	for _, c := range cookies {
		q.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	return w
}
func TestChallengeCookiePolicy(t *testing.T) {
	_, r, _, s, _ := transportFixture(t)
	w := request(r, "GET", "/api/v1/auth/login", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		CSRFToken string `json:"csrf_token"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	if !s.VerifyChallenge(result.CSRFToken) {
		t.Fatal("challenge signature")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-osc_login_csrf" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 300 {
		t.Fatal(c)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable secret")
	}
}

func TestSessionCookieIdleExpiryAndRefresh(t *testing.T) {
	_, r, f, _, raw := transportFixture(t)
	for _, absolute := range []time.Duration{time.Hour, 10 * time.Minute} {
		f.session.ExpiresAt = f.session.LastSeenAt.Add(absolute)
		w := request(r, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: raw})
		if w.Code != 200 {
			t.Fatal("valid session refused", w.Code)
		}
		want := f.session.LastSeenAt.Add(30 * time.Minute)
		if f.session.ExpiresAt.Before(want) {
			want = f.session.ExpiresAt
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != SessionCookie || !cookies[0].Expires.Equal(want) || cookies[0].Value != raw || cookies[0].MaxAge != int(want.Sub(f.session.LastSeenAt)/time.Second) {
			t.Fatal("idle cookie missing, reused beyond idle or extended beyond absolute")
		}
	}
}
func TestLoginCSRFAdmission(t *testing.T) {
	for _, tc := range []struct{ name, origin, header, cookie string }{{"missing", "https://admin.example.test", "", ""}, {"unsigned", "https://admin.example.test", "plain", "plain"}, {"foreign", "https://evil.example.test", "", ""}, {"null", "null", "", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			_, r, f, _, _ := transportFixture(t)
			q := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"email":"missing@example.test","password":"correct-password"}`))
			q.RemoteAddr = "192.0.2.1:1"
			q.Header.Set("Origin", tc.origin)
			q.Header.Set("Content-Type", "application/json")
			q.Header.Set("X-CSRF-Token", tc.header)
			if tc.cookie != "" {
				q.AddCookie(&http.Cookie{Name: "__Host-osc_login_csrf", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != 403 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.saved != 0 || f.failures != 0 {
				t.Fatal("csrf performed credential work")
			}
		})
	}
}
func TestLoginStrictJSON(t *testing.T) {
	cases := []struct {
		name, body, ctype string
		status            int
	}{{"unknown", `{"email":"a@example.test","password":"pw","extra":1}`, "application/json", 400}, {"second", `{} {}`, "application/json", 400}, {"oversize", `{"password":"` + strings.Repeat("x", 5000) + `"}`, "application/json", 413}, {"content", "{}", "text/plain", 415}, {"null", "null", "application/json", 400}, {"null-password", `{"email":"a@example.test","password":null}`, "application/json", 400}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r, f, s, _ := transportFixture(t)
			token, e := s.NewChallenge()
			if e != nil {
				t.Fatal(e)
			}
			q := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(tc.body))
			q.RemoteAddr = "192.0.2.1:1"
			q.Header.Set("Origin", "https://admin.example.test")
			q.Header.Set("Content-Type", tc.ctype)
			q.Header.Set("X-CSRF-Token", token)
			q.AddCookie(&http.Cookie{Name: "__Host-osc_login_csrf", Value: token})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.failures != 0 {
				t.Fatal("invalid DTO reached password verification")
			}
		})
	}
}
func TestSessionLogoutCSRFAndRevocation(t *testing.T) {
	_, r, f, s, raw := transportFixture(t)
	cookie := &http.Cookie{Name: "__Host-osc_admin_session", Value: raw}
	w := request(r, "GET", "/api/v1/auth/session", "", cookie)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), raw) {
		t.Fatal("raw session in body")
	}
	w = request(r, "POST", "/api/v1/auth/logout", "", cookie)
	if w.Code != 403 || f.revoked != 0 {
		t.Fatal(w.Code, f.revoked)
	}
	q := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	q.AddCookie(cookie)
	q.Header.Set("Origin", "https://admin.example.test")
	q.Header.Set("X-CSRF-Token", s.SessionCSRF(raw))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, q)
	if w.Code != 204 || f.revoked != 1 {
		t.Fatal(w.Code, w.Body.String(), f.revoked)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].MaxAge != -1 || !cs[0].Secure || !cs[0].HttpOnly || cs[0].Domain != "" {
		t.Fatal(cs)
	}
	w = request(r, "GET", "/api/v1/auth/session", "", cookie)
	if w.Code != 401 {
		t.Fatal("revoked session accepted", w.Code)
	}
}
func TestSessionCookieAmbiguityAndForeignWrite(t *testing.T) {
	_, r, f, s, raw := transportFixture(t)
	for _, tc := range []struct {
		name               string
		duplicate, foreign bool
	}{{"duplicate", true, false}, {"foreign", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			q := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
			q.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: raw})
			if tc.duplicate {
				q.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: raw})
			}
			origin := "https://admin.example.test"
			if tc.foreign {
				origin = "https://evil.example.test"
			}
			q.Header.Set("Origin", origin)
			q.Header.Set("X-CSRF-Token", s.SessionCSRF(raw))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != 401 && w.Code != 403 {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.revoked != 0 {
				t.Fatal("forgery revoked session")
			}
		})
	}
}
func TestAuthErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{login.ErrCredentials, 401}, {login.ErrSession, 401}, {login.ErrRateLimited, 429}, {login.ErrDatabase, 500}, {errors.New("secret DSN"), 500}} {
		w := httptest.NewRecorder()
		q := httptest.NewRequest("POST", "/", nil)
		writeError(w, q, tc.err)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "secret DSN") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestRouteClassesAndDefaultDeny(t *testing.T) {
	h, _, _, _, _ := transportFixture(t)
	for _, route := range h.Routes() {
		want := platform.AdminSession
		if route.Spec.OperationID == "createAdminSession" || route.Spec.OperationID == "getAdminLoginChallenge" {
			want = platform.Public
		}
		if route.Spec.Access != want || route.Spec.Scope != "" {
			t.Fatal("route admission changed", route.Spec)
		}
		r := platform.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if e := r.Register(route.Spec, route.Handler); e != nil {
			t.Fatal(e)
		}
		if want == platform.AdminSession {
			w := request(r, route.Spec.Method, route.Spec.Pattern, "")
			if w.Code != 401 {
				t.Fatal("private route without authenticator reachable", w.Code)
			}
		}
	}
}
func TestUnsupportedPrivateAccessDenied(t *testing.T) {
	h, _, _, _, raw := transportFixture(t)
	for _, spec := range []platform.RouteSpec{{OperationID: "foreign", Method: "GET", Pattern: "/foreign", Access: platform.AdminSession}, {OperationID: "getAdminSession", Method: "GET", Pattern: "/api/v1/auth/session", Access: platform.APIKey}, {OperationID: "getAdminSession", Method: "GET", Pattern: "/api/v1/auth/session", Access: platform.AdminSession, Scope: "catalog.write"}} {
		q := httptest.NewRequest("GET", "/", nil)
		q.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
		if ctx, e := h.Authenticate(q.Context(), q, spec); e == nil || ctx != nil {
			t.Fatal("unimplemented authentication capability admitted")
		}
	}
}
