// H-4 policy tests bind session admission and DB clock semantics to HTTP.
// Dependencies: real router/service plus bounded repository double; CI only.
// Invariants: deadlines release capacity, IP/global budgets are separate from login.
package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

func TestH4SessionRateWindows(t *testing.T) {
	for _, policy := range []string{"ip", "global", "ipv6"} {
		t.Run(policy, func(t *testing.T) {
			_, s, f, _ := h4Fixture(t, func(c *login.Config) {
				c.SessionIPBurst = 1
				if policy == "global" {
					c.SessionIPBurst = 10
					c.SessionGlobalBurst = 1
				}
			})
			ip1, ip2 := netip.MustParseAddr("192.0.2.5"), netip.MustParseAddr("192.0.2.5")
			if policy == "global" {
				ip2 = netip.MustParseAddr("192.0.2.6")
			}
			if policy == "ipv6" {
				ip1 = netip.MustParseAddr("2001:db8::1")
				ip2 = netip.MustParseAddr("2001:db8::2")
			}
			token := base64.RawURLEncoding.EncodeToString(secretKey(t))
			if _, _, err := s.Authenticate(context.Background(), token, ip1); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Authenticate(context.Background(), token, ip2); !errors.Is(err, login.ErrRateLimited) {
				t.Fatal("second query admitted", err)
			}
			if f.lookupCalls.Load() != 1 {
				t.Fatal("rate rejected query touched database")
			}
		})
	}
}

func TestH4SessionDeadlineReleasesAdmission(t *testing.T) {
	_, s, f, _ := h4Fixture(t, func(c *login.Config) { c.SessionQueryTimeout = 10 * time.Millisecond; c.SessionQuerySlots = 1 })
	f.entered, f.release = make(chan struct{}, 2), make(chan struct{})
	defer close(f.release)
	token := base64.RawURLEncoding.EncodeToString(secretKey(t))
	for i := 0; i < 2; i++ {
		if _, _, err := s.Authenticate(context.Background(), token); !errors.Is(err, login.ErrDatabase) {
			t.Fatal("query deadline did not fail closed", err)
		}
	}
	if f.lookupCalls.Load() != 2 {
		t.Fatal("deadline leaked admission slot")
	}
}

func TestH4SessionUsesDatabaseClockDespiteHostSkew(t *testing.T) {
	for _, offset := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		r, _, f, _ := h4Fixture(t, func(c *login.Config) { c.Now = func() time.Time { return time.Now().Add(offset) } })
		token := base64.RawURLEncoding.EncodeToString(secretKey(t))
		w := request(r, "GET", "/api/v1/auth/session", "", &http.Cookie{Name: SessionCookie, Value: token})
		if w.Code != 200 || f.lookupCalls.Load() != 1 {
			t.Fatal("host skew controlled session admission", w.Code)
		}
	}
}

func TestH4ConfiguredDeviceCookieLifetime(t *testing.T) {
	r, s, _, input := h4Fixture(t, func(c *login.Config) { c.DeviceLifetime = 24 * time.Hour })
	q := signedLogin(t, s, h4Email, input)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == DeviceCookie {
			if c.MaxAge != 24*60*60 {
				t.Fatal("configured device lifetime ignored")
			}
			return
		}
	}
	t.Fatal("device cookie missing")
}

func TestH4UnknownAccountWithOldCookieStillUsesCredentialFailure(t *testing.T) {
	r, s, f, input := h4Fixture(t)
	f.credential = login.Credential{}
	old := base64.RawURLEncoding.EncodeToString(secretKey(t))
	q := signedLogin(t, s, h4Email, input)
	q.AddCookie(&http.Cookie{Name: SessionCookie, Value: old})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, q)
	if w.Code != 401 || f.rotationCalls.Load() != 0 || len(w.Result().Cookies()) != 0 {
		t.Fatal("old cookie revealed account existence or changed cookie", w.Code)
	}
}
