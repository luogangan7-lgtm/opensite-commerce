// F-28 transport gates bind signed-CSRF, recovery cookies and proxy trust to
// the production handler. Dependencies: stdlib and existing login test doubles.
// Root calls these gates; no real identity data is touched.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
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
	"golang.org/x/crypto/argon2"
)

func signedLogin(t *testing.T, s *login.Service, email, password string) *http.Request {
	t.Helper()
	token, err := s.NewChallenge()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	q := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(string(body)))
	q.RemoteAddr = "192.0.2.1:54321"
	q.Header.Set("Origin", "https://admin.example.test")
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("X-CSRF-Token", token)
	q.AddCookie(&http.Cookie{Name: ChallengeCookie, Value: token})
	return q
}
func successfulCredential() login.Credential {
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte("correct-password"), salt, 2, 19456, 1, 32)
	return login.Credential{UserID: "user", Status: "active", PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)}
}
func TestF28SignedLoginGuards(t *testing.T) {
	for _, name := range []string{"missing-header", "mismatched-header", "foreign-origin", "password-1025"} {
		t.Run(name, func(t *testing.T) {
			_, r, f, s, _ := transportFixture(t)
			password := "correct-password"
			want := 403
			if name == "password-1025" {
				password = strings.Repeat("x", 1025)
				want = 400
			}
			q := signedLogin(t, s, "a@example.test", password)
			switch name {
			case "missing-header":
				q.Header.Del("X-CSRF-Token")
			case "mismatched-header":
				q.Header.Set("X-CSRF-Token", "mismatch")
			case "foreign-origin":
				q.Header.Set("Origin", "https://evil.example.test")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != want || f.credentials != 0 || f.saved != 0 || f.failures != 0 {
				t.Fatalf("guard status=%d credential=%d saved=%d failures=%d", w.Code, f.credentials, f.saved, f.failures)
			}
		})
	}
}
func TestF28LoginSessionCookies(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "lost-rotation"}[fail], func(t *testing.T) {
			_, r, f, s, raw := transportFixture(t)
			f.credential = successfulCredential()
			if fail {
				f.saveErr = login.ErrSession
			}
			q := signedLogin(t, s, "a@example.test", "correct-password")
			q.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			want := 201
			if fail {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			var session *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == SessionCookie {
					session = c
				}
			}
			if session == nil {
				t.Fatal("session cookie missing")
			}
			if fail {
				if session.Value != "" || session.MaxAge != -1 {
					t.Fatal("invalid session retained", session)
				}
			} else if session.MaxAge != 1800 || session.Value == "" {
				t.Fatal("live cookie lacks relative idle lifespan", session)
			}
		})
	}
}
func TestF28ClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:1::/48")}
	for _, tc := range []struct {
		name, peer, xff, want string
		trust, bad            bool
	}{
		{"default-none", "10.0.0.1:12", "203.0.113.3", "10.0.0.1", false, false},
		{"untrusted", "192.0.2.1:12", "garbage", "192.0.2.1", true, false},
		{"single", "10.0.0.1:12", "203.0.113.3", "203.0.113.3", true, false},
		{"spoof-leftmost", "10.0.0.1:12", "198.51.100.9, 203.0.113.3, 10.1.1.1", "203.0.113.3", true, false},
		{"ipv6", "[2001:db8:1::1]:12", "2001:db8:2::2", "2001:db8:2::2", true, false},
		{"mapped", "[::ffff:10.0.0.1]:12", "::ffff:203.0.113.3", "203.0.113.3", true, false},
		{"no-header", "10.0.0.1:12", "", "10.0.0.1", true, false},
		{"bad-hop", "10.0.0.1:12", "bad, 203.0.113.3", "", true, true},
		{"empty-hop", "10.0.0.1:12", "203.0.113.3,", "", true, true},
		{"with-port", "10.0.0.1:12", "203.0.113.3:123", "", true, true},
		{"zone", "10.0.0.1:12", "fe80::1%en0", "", true, true},
		{"too-long", "10.0.0.1:12", strings.Repeat(" ", 4097), "", true, true},
		{"too-many", "10.0.0.1:12", strings.Repeat("10.0.0.1,", 32) + "203.0.113.3", "", true, true},
		{"bad-peer", "bad", "203.0.113.3", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, s, _ := transportFixture(t)
			if tc.trust {
				h = New(s, "https://admin.example.test", 30*time.Minute, trusted...)
			}
			q := httptest.NewRequest("POST", "/", nil)
			q.RemoteAddr = tc.peer
			if tc.xff != "" {
				q.Header.Set("X-Forwarded-For", tc.xff)
			}
			ip, err := h.clientIP(q)
			if tc.bad {
				if err == nil {
					t.Fatal("malformed trusted chain admitted", ip)
				}
				return
			}
			if err != nil || ip.String() != tc.want {
				t.Fatal(ip, err, "want", tc.want)
			}
		})
	}
}
func TestF28MalformedTrustedChainRejectsBeforeCredential(t *testing.T) {
	_, _, f, s, _ := transportFixture(t)
	h := New(s, "https://admin.example.test", 30*time.Minute, netip.MustParsePrefix("10.0.0.0/8"))
	r := platform.New(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range h.Routes() {
		if err := r.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	q := signedLogin(t, s, "a@example.test", "correct-password")
	q.RemoteAddr = "10.0.0.1:1"
	q.Header.Set("X-Forwarded-For", "invalid")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	if w.Code != 400 || f.credentials != 0 {
		t.Fatal(w.Code, f.credentials)
	}
}
func TestF28GenericCredentialsAndRetry(t *testing.T) {
	credential := successfulCredential()
	for _, mode := range []string{"credentials", "account", "account-ip", "ip", "global"} {
		var shape string
		for _, exists := range []bool{false, true} {
			_, r, f, s, _ := transportFixture(t)
			if exists {
				f.credential = credential
			}
			now := f.session.LastSeenAt
			if mode == "account" {
				f.accountCooldown = now.Add(time.Minute)
			}
			if mode == "account-ip" {
				f.pairCooldown = now.Add(time.Minute)
			}
			if mode == "ip" || mode == "global" {
				// Same public admission setup for unknown/existing identities.
				cfg := login.Config{Origin: "https://admin.example.test", SigningKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now }, IPBurst: 1, GlobalBurst: 300}
				if mode == "global" {
					cfg.IPBurst = 30
					cfg.GlobalBurst = 2
				}
				var err error
				s, err = login.New(f, cfg)
				if err != nil {
					t.Fatal(err)
				}
				h := New(s, cfg.Origin, 30*time.Minute)
				r = platform.New(h, nil)
				for _, route := range h.Routes() {
					if err := r.Register(route.Spec, route.Handler); err != nil {
						t.Fatal(err)
					}
				}
				q := signedLogin(t, s, "a@example.test", "wrong")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, q)
			}
			q := signedLogin(t, s, "a@example.test", "wrong")
			if mode == "global" {
				q.RemoteAddr = "192.0.2.2:1"
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			want := 429
			retry := "60"
			if mode == "credentials" {
				want = 401
				retry = ""
			}
			if w.Code != want || w.Header().Get("Retry-After") != retry || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatal(mode, w.Code, w.Body.String(), w.Header())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			delete(body, "request_id")
			b, _ := json.Marshal(body)
			if shape == "" {
				shape = string(b)
			} else if shape != string(b) {
				t.Fatal("existence leaked", mode, shape, string(b))
			}
		}
	}
}

func TestF28StaleCookieRecovery(t *testing.T) {
	for _, kind := range []string{"malformed", "revoked", "expired", "other-user"} {
		t.Run(kind, func(t *testing.T) {
			_, r, f, s, raw := transportFixture(t)
			f.credential = successfulCredential()
			f.saveErr = login.ErrSession
			f.dead = true // Preflight observes stale authority, so SaveLogin receives no old token.
			if kind == "malformed" {
				raw = "malformed"
			}
			q := signedLogin(t, s, "a@example.test", "correct-password")
			q.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != 201 {
				t.Fatal("stale cookie blocked fresh credential login", w.Code)
			}
			var cleared bool
			for _, c := range w.Result().Cookies() {
				if c.Name == SessionCookie && c.Value == "" && c.MaxAge == -1 {
					cleared = true
				}
			}
			if cleared {
				t.Fatal("credential login emitted a deletion cookie")
			}
			// Successful credential login also works without an old cookie.
			q = signedLogin(t, s, "a@example.test", "correct-password")
			w = httptest.NewRecorder()
			r.ServeHTTP(w, q)
			if w.Code != 201 {
				t.Fatal("cleared cookie prevented recovery", w.Code, w.Body.String())
			}
		})
	}
}
func TestF28RepeatedXFFFields(t *testing.T) {
	h, _, _, s, _ := transportFixture(t)
	h = New(s, "https://admin.example.test", 30*time.Minute, netip.MustParsePrefix("10.0.0.0/8"))
	q := httptest.NewRequest("POST", "/", nil)
	q.RemoteAddr = "10.0.0.1:1"
	q.Header.Add("X-Forwarded-For", "198.51.100.9")
	q.Header.Add("X-Forwarded-For", "203.0.113.3, 10.1.1.1")
	ip, err := h.clientIP(q)
	if err != nil || ip.String() != "203.0.113.3" {
		t.Fatal(ip, err)
	}
}
