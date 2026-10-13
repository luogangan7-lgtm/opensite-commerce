// Package customerhttp adapts customer authentication at the platform boundary.
// Dependencies: public customer port and strict shared HTTP admission; bootstrap
// mounts these routes. Invariants: independent Host cookie, exact Origin and
// tenant-bound signed CSRF; staff identity and client-supplied IDs never grant access.
package customerhttp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

const SessionCookie = "__Host-osc_customer_session"
const ChallengeCookie = "__Host-osc_customer_challenge"

type Config struct {
	Service c.Service
	Origin  string
	Secret  []byte
	Now     func() time.Time
}
type Handler struct {
	cfg        Config
	configured bool
}

func New(cfg Config) *Handler {
	u, e := url.Parse(cfg.Origin)
	configured := e == nil && u.Scheme == "https" && u.Hostname() != "" && !u.ForceQuery && u.Opaque == "" && u.String() == cfg.Origin && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && len(cfg.Secret) >= 32 && cfg.Service != nil && !strings.ContainsAny(cfg.Origin, "\r\n")
	cfg.Secret = append([]byte(nil), cfg.Secret...)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Handler{cfg: cfg, configured: configured}
}
func (h *Handler) Routes() []httpapi.Route {
	const base = "/api/v1/t/{tenant_id}/customer"
	items := []struct{ id, method, path string }{
		{"registerCustomer", "POST", base + "/registrations"}, {"verifyCustomerEmail", "POST", base + "/verifications"},
		{"getCustomerSession", "GET", base + "/session"}, {"createCustomerSession", "POST", base + "/session"}, {"deleteCustomerSession", "DELETE", base + "/session"},
		{"requestCustomerPasswordReset", "POST", base + "/password-resets"}, {"confirmCustomerPasswordReset", "POST", base + "/password-resets/confirm"},
	}
	routes := make([]httpapi.Route, 0, len(items))
	for _, item := range items {
		operation := item.id
		routes = append(routes, httpapi.Route{Spec: httpapi.RouteSpec{OperationID: operation, Method: item.method, Pattern: item.path, Access: httpapi.Public, MaxBodySize: 4096}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, operation) })})
	}
	return routes
}
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, operation string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	fail := func(e error) { h.failure(w, r, e) }
	if !h.configured {
		fail(c.ErrUnavailable)
		return
	}
	tenant := r.PathValue("tenant_id")
	if !tenanttx.ValidTenantID(tenant) {
		fail(c.ErrInvalidInput)
		return
	}
	token, e := cookie(r, SessionCookie)
	if e != nil {
		fail(c.ErrUnauthorized)
		return
	}
	if operation == "getCustomerSession" {
		if len(r.Header.Values("Origin")) > 0 && !singleHeader(r, "Origin", h.cfg.Origin) {
			fail(apperrors.New(403, "csrf_failed", "Origin is not allowed"))
			return
		}
		if token == "" {
			challenge, e := h.challenge(tenant)
			if e != nil {
				fail(c.ErrUnavailable)
				return
			}
			setCookie(w, ChallengeCookie, challenge, h.cfg.Now().Add(5*time.Minute))
			writeJSON(w, 200, map[string]any{"customer": nil, "csrf_token": challenge})
			return
		}
		current, e := h.cfg.Service.Current(r.Context(), tenant, token)
		if e != nil {
			clearCookie(w, SessionCookie)
			fail(e)
			return
		}
		setCookie(w, SessionCookie, token, current.ExpiresAt)
		writeJSON(w, 200, map[string]any{"customer": current, "csrf_token": h.sessionCSRF(tenant, token)})
		return
	}
	// Exact Origin and signed CSRF admission precede JSON and all domain work.
	if !singleHeader(r, "Origin", h.cfg.Origin) || !h.writeCSRF(r, tenant, token) {
		fail(apperrors.New(403, "csrf_failed", "Origin and signed CSRF token are required"))
		return
	}
	if token != "" {
		if _, e := h.cfg.Service.Current(r.Context(), tenant, token); e != nil {
			if errors.Is(e, c.ErrUnauthorized) {
				clearCookie(w, SessionCookie)
			}
			fail(e)
			return
		}
	}
	if operation == "deleteCustomerSession" {
		if token == "" {
			fail(c.ErrUnauthorized)
			return
		}
		if e := h.cfg.Service.Logout(r.Context(), tenant, token); e != nil {
			fail(e)
			return
		}
		clearCookie(w, SessionCookie)
		clearCookie(w, ChallengeCookie)
		w.WriteHeader(204)
		return
	}
	keys := []string{"email", "password"}
	switch operation {
	case "verifyCustomerEmail":
		keys = []string{"token"}
	case "requestCustomerPasswordReset":
		keys = []string{"email"}
	case "confirmCustomerPasswordReset":
		keys = []string{"token", "password"}
	}
	fields, e := body(w, r, keys)
	if e != nil {
		fail(e)
		return
	}
	ip, e := remoteIP(r)
	if e != nil {
		fail(c.ErrInvalidInput)
		return
	}
	switch operation {
	case "registerCustomer":
		e = h.cfg.Service.Register(r.Context(), tenant, fields["email"], fields["password"], ip)
		if e == nil {
			writeJSON(w, 202, map[string]string{"status": "verification_pending"})
		}
	case "verifyCustomerEmail":
		e = h.cfg.Service.Verify(r.Context(), tenant, fields["token"])
		if e == nil {
			w.WriteHeader(204)
		}
	case "requestCustomerPasswordReset":
		e = h.cfg.Service.RequestReset(r.Context(), tenant, fields["email"], ip)
		if e == nil {
			writeJSON(w, 202, map[string]string{"status": "accepted"})
		}
	case "confirmCustomerPasswordReset":
		e = h.cfg.Service.Reset(r.Context(), tenant, fields["token"], fields["password"], ip)
		if e == nil {
			clearCookie(w, SessionCookie)
			clearCookie(w, ChallengeCookie)
			w.WriteHeader(204)
		}
	case "createCustomerSession":
		var session c.Session
		session, e = h.cfg.Service.Login(r.Context(), tenant, fields["email"], fields["password"], ip, token)
		if e == nil {
			setCookie(w, SessionCookie, session.Token, session.ExpiresAt)
			clearCookie(w, ChallengeCookie)
			writeJSON(w, 200, map[string]any{"customer": session, "csrf_token": h.sessionCSRF(tenant, session.Token)})
		}
	}
	if e != nil {
		fail(e)
	}
}
func body(w http.ResponseWriter, r *http.Request, keys []string) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if e := httpapi.DecodeJSON(w, r, &raw); e != nil {
		return nil, e
	}
	if len(raw) != len(keys) {
		return nil, apperrors.New(400, "invalid_json", "Canonical authentication fields are required")
	}
	fields := make(map[string]string, len(keys))
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return nil, apperrors.New(400, "invalid_json", "Canonical authentication fields are required")
		}
		var text string
		if e := json.Unmarshal(v, &text); e != nil {
			return nil, apperrors.New(400, "invalid_json", "Authentication fields must be strings")
		}
		fields[key] = text
	}
	return fields, nil
}
func cookie(r *http.Request, name string) (string, error) {
	var value string
	count := 0
	for _, item := range r.Cookies() {
		if item.Name == name {
			count++
			value = item.Value
		}
	}
	if count > 1 || count == 1 && value == "" {
		return "", c.ErrUnauthorized
	}
	return value, nil
}
func singleHeader(r *http.Request, key, want string) bool {
	v := r.Header.Values(key)
	return len(v) == 1 && hmac.Equal([]byte(v[0]), []byte(want))
}
func (h *Handler) mac(value string) string {
	m := hmac.New(sha256.New, h.cfg.Secret)
	m.Write([]byte("customer-http:" + h.cfg.Origin + ":" + value))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
func (h *Handler) sessionCSRF(tenant, token string) string {
	return h.mac("customer-session:" + tenant + ":" + token)
}
func (h *Handler) challenge(tenant string) (string, error) {
	nonce := make([]byte, 32)
	if _, e := rand.Read(nonce); e != nil {
		return "", e
	}
	payload := strconv.FormatInt(h.cfg.Now().Add(5*time.Minute).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	return payload + "." + h.mac("customer-challenge:"+tenant+":"+payload), nil
}
func (h *Handler) writeCSRF(r *http.Request, tenant, token string) bool {
	values := r.Header.Values("X-CSRF-Token")
	if len(values) != 1 {
		return false
	}
	if token != "" {
		return singleHeader(r, "X-CSRF-Token", h.sessionCSRF(tenant, token))
	}
	value, e := cookie(r, ChallengeCookie)
	if e != nil || value == "" || !hmac.Equal([]byte(value), []byte(values[0])) {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	expiry, e := strconv.ParseInt(parts[0], 10, 64)
	now := h.cfg.Now().Unix()
	if e != nil || expiry <= now || expiry > now+300 {
		return false
	}
	nonce, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != parts[1] {
		return false
	}
	return hmac.Equal([]byte(parts[2]), []byte(h.mac("customer-challenge:"+tenant+":"+parts[0]+"."+parts[1])))
}
func remoteIP(r *http.Request) (netip.Addr, error) {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return netip.Addr{}, e
	}
	return netip.ParseAddr(host)
}
func setCookie(w http.ResponseWriter, name, value string, expiry time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Expires: expiry, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/"})
}
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/"})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Handler) failure(w http.ResponseWriter, r *http.Request, e error) {
	var app *apperrors.Error
	if errors.As(e, &app) {
		httpapi.WriteProblem(w, r, e)
		return
	}
	var limited *c.RateLimitError
	if errors.As(e, &limited) {
		seconds := int64((limited.RetryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		httpapi.WriteProblem(w, r, apperrors.New(429, "rate_limited", "Try again later"))
		return
	}
	status, code, detail := 503, "customer_auth_unavailable", "Customer authentication is unavailable"
	switch {
	case errors.Is(e, c.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		status, code, detail = 429, "rate_limited", "Try again later"
	case errors.Is(e, c.ErrInvalidCredentials):
		status, code, detail = 401, "invalid_credentials", "Invalid email or password"
	case errors.Is(e, c.ErrUnauthorized):
		status, code, detail = 401, "unauthenticated", "Customer authentication is required"
	case errors.Is(e, c.ErrToken):
		status, code, detail = 400, "invalid_token", "Invalid or expired token"
	case errors.Is(e, c.ErrInvalidInput):
		status, code, detail = 400, "invalid_request", "Invalid authentication input"
	}
	httpapi.WriteProblem(w, r, apperrors.New(status, code, detail))
}

type invalidCookieKey struct{}

// WrapTransport lets admission clear a conclusively invalid cookie even when
// a downstream cart/checkout capability rejects the request. The callback is
// private to this request; it never grants an actor or catches a database error.
func (h *Handler) WrapTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), invalidCookieKey{}, func() { clearCookie(w, SessionCookie) })
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AdmitGuest requires the tenant-bound anonymous challenge for guest checkout.
// A stale customer bearer never serves as that proof; cart's own guest token
// must also be authenticated by the caller before constructing a guest actor.
func (h *Handler) AdmitGuest(r *http.Request) error {
	if !h.configured {
		return apperrors.New(503, "customer_auth_unavailable", "Customer authentication is unavailable")
	}
	if !singleHeader(r, "Origin", h.cfg.Origin) || !h.writeCSRF(r, r.PathValue("tenant_id"), "") {
		return apperrors.New(403, "csrf_failed", "Origin and signed CSRF token are required")
	}
	return nil
}

// Resolve rechecks the live customer session; callers cannot mint an actor from
// IDs in JSON. Only a definitively invalid bearer falls back to guest capability
// admission. A transient store failure never changes identity or the cookie.
func (h *Handler) Resolve(ctx context.Context, r *http.Request) (c.Actor, error) {
	token, e := cookie(r, SessionCookie)
	if e != nil {
		return c.Actor{}, apperrors.New(401, "unauthenticated", "Customer authentication is required")
	}
	if token == "" {
		return c.Actor{}, nil
	}
	if !h.configured {
		return c.Actor{}, apperrors.New(503, "customer_auth_unavailable", "Customer authentication is unavailable")
	}
	tenant := r.PathValue("tenant_id")
	if !tenanttx.ValidTenantID(tenant) {
		return c.Actor{}, apperrors.New(404, "not_found", "Resource not found")
	}
	write := r.Method != "GET" && r.Method != "HEAD"
	if write && !singleHeader(r, "Origin", h.cfg.Origin) {
		return c.Actor{}, apperrors.New(403, "csrf_failed", "Origin and signed CSRF token are required")
	}
	session, e := h.cfg.Service.Current(ctx, tenant, token)
	if e != nil {
		if errors.Is(e, c.ErrUnauthorized) {
			if clear, ok := ctx.Value(invalidCookieKey{}).(func()); ok {
				clear()
			}
			return c.Actor{}, nil
		}
		return c.Actor{}, apperrors.New(503, "customer_auth_unavailable", "Customer authentication is unavailable")
	}
	if write && !h.writeCSRF(r, tenant, token) {
		return c.Actor{}, apperrors.New(403, "csrf_failed", "Origin and signed CSRF token are required")
	}
	return c.Actor{TenantID: session.TenantID, CustomerID: session.CustomerID, SessionID: session.ID}, nil
}
