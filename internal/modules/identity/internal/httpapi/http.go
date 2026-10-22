// Package httpapi adapts staff login/session services to strict platform routes.
// Dependencies: identity login and platform HTTP/errors; static bootstrap calls it.
// Invariants: exact Origin plus signed/session-bound CSRF before credential writes;
// cookies carry only opaque tokens, and private unsupported routes stay denied.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const (
	SessionCookie   = "__Host-osc_admin_session"
	DeviceCookie    = "__Host-osc_known_device"
	ChallengeCookie = "__Host-osc_login_csrf"
)

type Handler struct {
	service       *login.Service
	origin        string
	idle          time.Duration
	trusted       []netip.Prefix
	authorization contract.AuthorizationRepository
}

func New(s *login.Service, origin string, idle time.Duration, trusted ...netip.Prefix) *Handler {
	return &Handler{service: s, origin: origin, idle: idle, trusted: append([]netip.Prefix(nil), trusted...)}
}
func (h *Handler) Routes() []platform.Route {
	return []platform.Route{
		{Spec: platform.RouteSpec{OperationID: "getAdminLoginChallenge", Method: "GET", Pattern: "/api/v1/auth/login", Access: platform.Public}, Handler: http.HandlerFunc(h.challenge)},
		{Spec: platform.RouteSpec{OperationID: "createStaffLoginSession", Method: "POST", Pattern: "/api/v1/auth/login", Access: platform.Public, MaxBodySize: 4096}, Handler: http.HandlerFunc(h.create)},
		{Spec: platform.RouteSpec{OperationID: "getAdminSession", Method: "GET", Pattern: "/api/v1/auth/session", Access: platform.AuthenticatedSelf}, Handler: http.HandlerFunc(h.session)},
		{Spec: platform.RouteSpec{OperationID: "logoutCurrentStaffSession", Method: "POST", Pattern: "/api/v1/auth/logout", Access: platform.AuthenticatedSelf}, Handler: http.HandlerFunc(h.logout)},
	}
}
func (h *Handler) sameOrigin(r *http.Request) bool { return oneHeader(r, "Origin") == h.origin }
func oneHeader(r *http.Request, name string) string {
	v := r.Header.Values(name)
	if len(v) != 1 {
		return ""
	}
	return v[0]
}
func cookieValue(r *http.Request, name string) (string, bool) {
	var value string
	found := false
	for _, c := range r.Cookies() {
		if c.Name == name {
			if found {
				return "", false
			}
			found = true
			value = c.Value
		}
	}
	return value, true
}
func (h *Handler) challenge(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" && !h.sameOrigin(r) {
		writeError(w, r, apperrors.New(403, "csrf_invalid", "Request authenticity could not be verified"))
		return
	}
	token, err := h.service.NewChallenge()
	if err != nil {
		writeError(w, r, err)
		return
	}
	setCookie(w, ChallengeCookie, token, 300, time.Time{})
	writeJSON(w, 200, map[string]string{"csrf_token": token})
}
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	challenge, ok := cookieValue(r, ChallengeCookie)
	header := oneHeader(r, "X-CSRF-Token")
	if !h.sameOrigin(r) || !ok || !h.service.VerifyChallenge(challenge) || subtle.ConstantTimeCompare([]byte(challenge), []byte(header)) != 1 {
		writeError(w, r, apperrors.New(403, "csrf_invalid", "Request authenticity could not be verified"))
		return
	}
	var dto struct {
		Email    *string `json:"email"`
		Password *string `json:"password"`
	}
	if err := platform.DecodeJSON(w, r, &dto); err != nil {
		writeError(w, r, err)
		return
	}
	if dto.Email == nil || dto.Password == nil || !login.ValidIdentifier(login.NormalizeIdentifier(*dto.Email)) || len(*dto.Password) == 0 || len(*dto.Password) > 1024 || !utf8.ValidString(*dto.Password) || !utf8.ValidString(*dto.Email) || strings.IndexFunc(*dto.Email, unicode.IsControl) >= 0 || strings.ContainsAny(*dto.Password, "\r\n\x00") {
		writeError(w, r, apperrors.New(400, "invalid_login_input", "Login input is invalid"))
		return
	}
	ip, err := h.clientIP(r)
	if err != nil {
		writeError(w, r, apperrors.New(400, "invalid_client_address", "Client address is invalid"))
		return
	}
	device, deviceOK := cookieValue(r, DeviceCookie)
	old, oldOK := cookieValue(r, SessionCookie)
	if !deviceOK || !oldOK {
		writeError(w, r, apperrors.New(400, "ambiguous_cookie", "Cookie input is invalid"))
		return
	}
	result, err := h.service.Login(r.Context(), login.LoginInput{Identifier: *dto.Email, Password: *dto.Password, DeviceToken: device, OldSessionToken: old, IP: ip.Unmap()})
	if err != nil {
		if errors.Is(err, login.ErrSession) {
			setCookie(w, SessionCookie, "", -1, time.Unix(1, 0))
		}
		writeError(w, r, err)
		return
	}
	h.setSessionCookie(w, result.SessionToken, result.Session)
	setCookie(w, DeviceCookie, result.DeviceToken, 90*24*60*60, time.Time{})
	setCookie(w, ChallengeCookie, "", -1, time.Unix(1, 0))
	writeJSON(w, 201, sessionBody(result.Session, result.CSRFToken))
}
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	spec := platform.RouteSpec{OperationID: "getAdminSession", Method: "GET", Pattern: "/api/v1/auth/session", Access: platform.AuthenticatedSelf}
	if _, err := h.admitted(r.Context(), r, spec); err != nil {
		writeError(w, r, err)
		return
	}
	p := r.Context().Value(principalKey{}).(principal)
	if _, _, err := h.service.Authenticate(r.Context(), p.token); err != nil {
		writeError(w, r, err)
		return
	}
	h.setSessionCookie(w, p.token, p.session)
	writeJSON(w, 200, sessionBody(p.session, h.service.SessionCSRF(p.token)))
}
func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, s login.Session) {
	expires := h.cookieExpiry(s)
	// The repository returns last_seen_at from its execution clock. Derive a
	// relative browser lifetime from that clock, never the HTTP host wall clock.
	maxAge := int(expires.Sub(s.LastSeenAt) / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	setCookie(w, SessionCookie, token, maxAge, expires)
}
func (h *Handler) cookieExpiry(s login.Session) time.Time {
	expires := s.LastSeenAt.Add(h.idle)
	if s.ExpiresAt.Before(expires) {
		expires = s.ExpiresAt
	}
	return expires
}
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	actor, ok := login.ActorFromContext(r.Context())
	if !ok {
		writeError(w, r, login.ErrSession)
		return
	}
	if err := h.service.Logout(r.Context(), actor); err != nil {
		writeError(w, r, err)
		return
	}
	setCookie(w, SessionCookie, "", -1, time.Unix(1, 0))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(204)
}
func sessionBody(s login.Session, csrf string) any {
	return struct {
		UserID    string    `json:"user_id"`
		SessionID string    `json:"session_id"`
		ExpiresAt time.Time `json:"expires_at"`
		CSRFToken string    `json:"csrf_token"`
	}{s.UserID, s.ID, s.ExpiresAt, csrf}
}
func setCookie(w http.ResponseWriter, name, value string, maxAge int, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: expires})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, login.ErrRateLimited) {
		w.Header().Set("Retry-After", "60")
	}
	platform.WriteProblem(w, r, mapError(err))
}
func mapError(err error) error {
	var existing *apperrors.Error
	if errors.As(err, &existing) {
		return err
	}
	switch {
	case errors.Is(err, login.ErrCredentials):
		return apperrors.New(401, "invalid_credentials", "Invalid credentials")
	case errors.Is(err, login.ErrSession):
		return apperrors.New(401, "unauthenticated", "Authentication is required")
	case errors.Is(err, login.ErrRateLimited):
		return apperrors.New(429, "auth_rate_limited", "Authentication is rate limited")
	default:
		return apperrors.New(503, "auth_unavailable", "Authentication is unavailable")
	}
}
