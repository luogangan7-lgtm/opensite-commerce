// Responsibilities: witness tenant admission renewing the browser session.
// Dependencies: real Router, login service and deterministic DB-result double.
// Invariants: renewal uses the returned activity stamp, never another credential
// class, a denied route or a lifetime beyond the session's absolute expiry.
package httpapi

import (
	"net/http"
	"testing"
	"time"

	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func TestC02cTenantSessionRenewsCookie(t *testing.T) {
	for _, access := range []platform.Access{platform.AdminSession, platform.AdminOrAPIKey} {
		t.Run(string(access), func(t *testing.T) {
			f := c02cAuthFixture(t)
			w := f.serve(t, c02cSpec(access, http.MethodGet, "catalog:read"), f.request(http.MethodGet, c02cTenant, true, false))
			c02cAssert(t, f, w, http.StatusOK, "user", c02cUser, c02cTenant)
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != SessionCookie {
				t.Fatalf("TENANT_COOKIE_RENEWAL_RED: admitted session did not renew: %d cookies", len(cookies))
			}
			c := cookies[0]
			if c.Value != f.sessionToken || c.Path != "/" || c.Domain != "" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatal("renewal changed host-cookie security policy or credential")
			}
			if c.MaxAge != 1800 || !c.Expires.Equal(f.sessions.session.LastSeenAt.Add(30*time.Minute)) {
				t.Fatal("cookie lifetime did not follow returned session activity stamp")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("personal session renewal response was cacheable")
			}
		})
	}
}

func TestC02cRenewalIsBoundedAndOnlyAdmittedSession(t *testing.T) {
	f := c02cAuthFixture(t)
	f.sessions.session.ExpiresAt = f.sessions.session.LastSeenAt.Add(5 * time.Minute)
	w := f.serve(t, c02cSpec(platform.AdminSession, http.MethodGet, "catalog:read"), f.request(http.MethodGet, c02cTenant, true, false))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != 300 || !cookies[0].Expires.Equal(f.sessions.session.ExpiresAt) {
		t.Fatal("TENANT_COOKIE_ABSOLUTE_RED: cookie renewal ignored absolute lifetime")
	}
	for _, tc := range []struct {
		name           string
		access         platform.Access
		tenant, scope  string
		cookie, bearer bool
		status         int
	}{
		{"bearer", platform.APIKey, c02cTenant, "catalog:read", false, true, 200},
		{"wrong-tenant", platform.AdminSession, c02cOtherTenant, "catalog:read", true, false, 404},
		{"missing-scope", platform.AdminSession, c02cTenant, "unknown:write", true, false, 403},
		{"missing-credential", platform.AdminSession, c02cTenant, "catalog:read", false, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := c02cAuthFixture(t)
			w := f.serve(t, c02cSpec(tc.access, http.MethodGet, tc.scope), f.request(http.MethodGet, tc.tenant, tc.cookie, tc.bearer))
			if w.Code != tc.status || len(w.Result().Cookies()) != 0 {
				t.Fatalf("non-session/denied request renewed cookie: status=%d", w.Code)
			}
		})
	}
}
