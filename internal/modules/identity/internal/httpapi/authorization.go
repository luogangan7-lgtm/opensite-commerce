// Responsibilities: admit session or API key credentials and project authority.
// Dependencies: login, neutral identity contracts and platform routes; bootstrap
// injects the tenant repository. Every request checks current membership; private
// route-bound context and transaction closures never derive authority from headers.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type principalKey struct{}
type principal struct {
	owner    *Handler
	spec     platform.RouteSpec
	snapshot AuthorizationSnapshot
	token    string
	session  login.Session
}

// AuthorizationSnapshot is a read-only projection of admitted authority. UserID
// is the session subject or API key issuer. Scopes are effective exact scopes;
// changing the returned copy cannot change admission or transaction authority.
type AuthorizationSnapshot struct {
	UserID, TenantID, Kind, CredentialID string
	Scopes                               []string
}

func AuthorizationFromContext(ctx context.Context) (AuthorizationSnapshot, bool) {
	if ctx == nil {
		return AuthorizationSnapshot{}, false
	}
	p, ok := ctx.Value(principalKey{}).(principal)
	if !ok || p.owner == nil {
		return AuthorizationSnapshot{}, false
	}
	s := p.snapshot
	s.Scopes = slices.Clone(s.Scopes)
	return s, true
}

// WithAuthorization is a startup-only builder, before serving concurrent requests.
func (h *Handler) WithAuthorization(repo contract.AuthorizationRepository) *Handler {
	h.authorization = repo
	return h
}

func unauthenticated() error {
	return apperrors.New(401, "unauthenticated", "Authentication is required")
}
func authUnavailable() error {
	return apperrors.New(503, "auth_unavailable", "Authentication is unavailable")
}
func scopeDenied() error {
	return apperrors.New(403, "scope_forbidden", "Required scope is unavailable")
}
func tenantNotFound() error { return apperrors.New(404, "not_found", "Tenant was not found") }

func selfRoute(spec platform.RouteSpec) bool {
	return spec.Access == platform.AuthenticatedSelf && spec.Scope == "" &&
		((spec.OperationID == "getAdminSession" && spec.Method == "GET" && spec.Pattern == "/api/v1/auth/session") ||
			(spec.OperationID == "logoutCurrentStaffSession" && spec.Method == "POST" && spec.Pattern == "/api/v1/auth/logout"))
}

func (h *Handler) Authenticate(ctx context.Context, r *http.Request, spec platform.RouteSpec) (context.Context, error) {
	self := selfRoute(spec)
	if !self && (spec.Access != platform.AdminSession && spec.Access != platform.APIKey && spec.Access != platform.AdminOrAPIKey || !strings.Contains(spec.Pattern, "{tenant_id}") || spec.Scope == "") {
		return nil, unauthenticated()
	}
	// Ambiguous credential classes never fall back to a different class.
	token, ok := cookieValue(r, SessionCookie)
	headers := r.Header.Values("Authorization")
	sessionSupplied := false
	for _, cookie := range r.Cookies() {
		if cookie.Name == SessionCookie {
			sessionSupplied = true
		}
	}
	if !ok || len(headers) > 1 || len(headers) != 0 && sessionSupplied {
		return nil, unauthenticated()
	}
	keyCredential := len(headers) == 1
	if keyCredential && (self || spec.Access == platform.AdminSession) || !keyCredential && spec.Access == platform.APIKey {
		return nil, unauthenticated()
	}
	if !keyCredential && token == "" {
		return nil, unauthenticated()
	}
	var keyID string
	var hash [32]byte
	if keyCredential {
		var valid bool
		keyID, hash, valid = parseKey(headers[0])
		if !valid {
			return nil, unauthenticated()
		}
	}
	// Cookie CSRF precedes even session activity refresh. Bearer credentials do
	// not use browser cookie authority and therefore do not require cookie CSRF.
	if !keyCredential && r.Method != "GET" && r.Method != "HEAD" {
		if h.service == nil {
			return nil, authUnavailable()
		}
		if !h.sameOrigin(r) || !h.service.VerifySessionCSRF(token, oneHeader(r, "X-CSRF-Token")) {
			return nil, apperrors.New(403, "csrf_invalid", "Request authenticity could not be verified")
		}
	}
	tenant := ""
	if !self {
		tenant = r.PathValue("tenant_id")
		if !tenanttx.ValidTenantID(tenant) {
			return nil, apperrors.New(400, "invalid_tenant", "Tenant must be a canonical UUID")
		}
		if h.authorization == nil {
			return nil, authUnavailable()
		}
	}
	snapshot := AuthorizationSnapshot{TenantID: tenant, Kind: contract.KindSession}
	var key contract.APIKey
	var session login.Session
	if keyCredential {
		var err error
		key, err = h.authorization.Key(ctx, keyID)
		if err != nil {
			return nil, authorityError(err, unauthenticated())
		}
		if key.ID != keyID || !tenanttx.ValidTenantID(key.UserID) || subtle.ConstantTimeCompare(hash[:], key.Hash[:]) != 1 {
			return nil, unauthenticated()
		}
		// This check precedes membership, including issuers belonging to both tenants.
		if key.TenantID != tenant {
			return nil, tenantNotFound()
		}
		snapshot.UserID, snapshot.CredentialID, snapshot.Kind = key.UserID, key.ID, contract.KindAPIKey
	} else {
		if h.service == nil {
			return nil, authUnavailable()
		}
		actor, liveSession, err := h.service.Authenticate(ctx, token)
		if err != nil {
			return nil, mapError(err)
		}
		if !tenanttx.ValidTenantID(actor.UserID()) || !tenanttx.ValidTenantID(actor.SessionID()) {
			return nil, unauthenticated()
		}
		snapshot.UserID, snapshot.CredentialID = actor.UserID(), actor.SessionID()
		ctx = login.WithActor(ctx, actor)
		session = liveSession
	}
	if !self {
		membership, err := h.authorization.Membership(ctx, snapshot.UserID, tenant)
		missing := tenantNotFound()
		if keyCredential {
			missing = unauthenticated()
		}
		if err != nil {
			return nil, authorityError(err, missing)
		}
		if membership.UserID != snapshot.UserID || membership.TenantID != tenant {
			return nil, missing
		}
		snapshot.Scopes = contract.RoleScopes(membership.Role)
		if keyCredential {
			snapshot.Scopes = slices.DeleteFunc(snapshot.Scopes, func(scope string) bool { return !slices.Contains(key.Scopes, scope) })
		}
		if !slices.Contains(snapshot.Scopes, spec.Scope) {
			return nil, scopeDenied()
		}
		if keyCredential {
			if err := h.authorization.TouchKey(ctx, key.ID); err != nil {
				return nil, authorityError(err, unauthenticated())
			}
		}
	}
	return context.WithValue(ctx, principalKey{}, principal{owner: h, spec: spec, snapshot: snapshot, token: token, session: session}), nil
}

// AuthenticationResponse slides only admitted tenant session cookies. Expires
// follows the actual DB activity stamp (which a busy touch may leave unchanged).
// A fresh fixed Max-Age would incorrectly extend a skipped touch's lifetime.
func (h *Handler) AuthenticationResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, spec platform.RouteSpec) {
	p, err := h.admitted(ctx, r, spec)
	if err != nil || p.snapshot.TenantID == "" || p.snapshot.Kind != contract.KindSession || p.token == "" || p.session.ID != p.snapshot.CredentialID {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	setCookie(w, SessionCookie, p.token, h.cookieExpiry(p.session), 0)
}

func parseKey(header string) (string, [32]byte, bool) {
	if len(header) != len("Bearer ")+36+1+43 || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return "", [32]byte{}, false
	}
	value := header[len("Bearer "):]
	id, encoded, ok := strings.Cut(value, ".")
	if !ok || !tenanttx.ValidTenantID(id) || len(encoded) != 43 {
		return "", [32]byte{}, false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != encoded {
		return "", [32]byte{}, false
	}
	return id, sha256.Sum256(secret), true
}

func authorityError(err, missing error) error {
	if errors.Is(err, contract.ErrNotFound) {
		return missing
	}
	var mapped *apperrors.Error
	if errors.As(err, &mapped) {
		// Tenant transaction capabilities deliberately classify 401/403/404.
		// Every other repository failure is sanitized at this boundary.
		switch mapped.Status {
		case 401:
			return unauthenticated()
		case 403:
			return scopeDenied()
		case 404:
			return tenantNotFound()
		}
	}
	return authUnavailable()
}

func (h *Handler) admitted(ctx context.Context, r *http.Request, spec platform.RouteSpec) (AuthorizationSnapshot, error) {
	if ctx == nil {
		return AuthorizationSnapshot{}, unauthenticated()
	}
	p, ok := ctx.Value(principalKey{}).(principal)
	if !ok || p.owner != h || p.spec != spec || p.snapshot.TenantID != r.PathValue("tenant_id") {
		return AuthorizationSnapshot{}, unauthenticated()
	}
	return p.snapshot, nil
}

func (h *Handler) ResolveActor(ctx context.Context, r *http.Request, spec platform.RouteSpec) (platform.Actor, error) {
	p, err := h.admitted(ctx, r, spec)
	if err != nil {
		return platform.Actor{}, err
	}
	if p.Kind == contract.KindAPIKey {
		return platform.Actor{Kind: "api_key", ID: p.CredentialID}, nil
	}
	return platform.Actor{Kind: "user", ID: p.UserID}, nil
}

// ResolveTransactionAuthorization captures the admitted values. Request headers
// and the context passed when the callback runs cannot replace that authority.
func (h *Handler) ResolveTransactionAuthorization(ctx context.Context, r *http.Request, spec platform.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	if !strings.Contains(spec.Pattern, "{tenant_id}") || spec.Access == platform.AuthenticatedSelf {
		return nil, nil
	}
	p, err := h.admitted(ctx, r, spec)
	if err != nil {
		return nil, err
	}
	if h.authorization == nil {
		return nil, authUnavailable()
	}
	revalidator, ok := h.authorization.(contract.TransactionAuthorizer)
	if !ok {
		return nil, authUnavailable()
	}
	grant := contract.TransactionAuthorization{UserID: p.UserID, Kind: p.Kind, CredentialID: p.CredentialID, Scope: spec.Scope}
	tenant := p.TenantID
	return func(txctx context.Context, q *db.Queries) error {
		if q == nil {
			return authUnavailable()
		}
		if err := revalidator.Revalidate(txctx, q, tenant, grant); err != nil {
			return authorityError(err, unauthenticated())
		}
		return nil
	}, nil
}
