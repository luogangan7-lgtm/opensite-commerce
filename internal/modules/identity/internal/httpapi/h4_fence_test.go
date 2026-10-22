// Responsibility: H4 credential-login response and configuration regressions.
// Dependencies: signed HTTP login fixtures; callers: CI Go gates.
// Invariants: failed rotation cannot erase a concurrent winner's cookie.
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestH4FenceLosingLoginEmitsNoCookies(t *testing.T) {
	_, router, repo, service, raw := transportFixture(t)
	repo.credential, repo.saveErr = successfulCredential(), login.ErrSession
	q := signedLogin(t, service, "a@example.test", "correct-password")
	q.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, q)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("rotation loser mutated winner cookies", w.Code, w.Header())
	}
}

func TestH4FenceUnknownAccountOldCookie(t *testing.T) {
	for _, old := range []string{"", "malformed", strings.Repeat("A", 43)} {
		_, router, repo, service, _ := transportFixture(t)
		q := signedLogin(t, service, "unknown@example.test", "wrong")
		if old != "" {
			q.AddCookie(&http.Cookie{Name: SessionCookie, Value: old})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, q)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_credentials") || repo.credentials != 1 || repo.failures != 1 || repo.saved != 0 || len(w.Result().Cookies()) != 0 {
			t.Fatal("old cookie changed uniform credential failure", w.Code, w.Body.String())
		}
	}
}

func TestH4FenceConfiguredDeviceCookie(t *testing.T) {
	_, _, repo, _, _ := transportFixture(t)
	repo.credential = successfulCredential()
	cfg := login.Config{Origin: "https://admin.example.test", SigningKey: []byte(strings.Repeat("k", 32)), DeviceLifetime: 7 * time.Hour}
	service, err := login.New(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(service, cfg.Origin, 30*time.Minute)
	router := platform.New(h, nil)
	for _, route := range h.Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, signedLogin(t, service, "a@example.test", "correct-password"))
	if w.Code != 201 {
		t.Fatal("configured-lifetime login failed", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == DeviceCookie {
			if c.MaxAge != 7*60*60 {
				t.Fatal("device cookie ignored configured lifetime", c.MaxAge)
			}
			return
		}
	}
	t.Fatal("missing device cookie")
}

func TestH4FenceCanonicalOrigin(t *testing.T) {
	_, _, repo, _, _ := transportFixture(t)
	if _, err := login.New(repo, login.Config{Origin: "https://admin.example.test:443", SigningKey: []byte(strings.Repeat("k", 32))}); err == nil {
		t.Fatal("explicit default port accepted as canonical Origin")
	}
}
