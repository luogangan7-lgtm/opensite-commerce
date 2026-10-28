// Package httpapi adapts tenant API-key management to centrally admitted routes.
// Dependencies: tenant contract, role-derived scope projection and platform stamps.
// Invariants: only AdminSession routes; DB rechecks authority, secret emitted once
// with no-store, no secret/list hash fields and no replay/idempotency secret cache.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	auth "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Handler struct {
	repository contract.Repository
	scopes     func(context.Context) []string
}

// scopes projects the authenticated session's role-derived scopes without IO.
func New(repository contract.Repository, scopes func(context.Context) []string) *Handler {
	return &Handler{repository: repository, scopes: scopes}
}
func (h *Handler) Routes() []platform.Route {
	base := "/api/v1/t/{tenant_id}/api-keys"
	return []platform.Route{
		{Spec: platform.RouteSpec{OperationID: "listAPIKeys", Method: http.MethodGet, Pattern: base, Access: platform.AdminSession, Scope: "apikeys:write"}, Handler: http.HandlerFunc(h.list)},
		{Spec: platform.RouteSpec{OperationID: "createAPIKey", Method: http.MethodPost, Pattern: base, Access: platform.AdminSession, Scope: "apikeys:write", MaxBodySize: 4096}, Handler: http.HandlerFunc(h.create)},
		{Spec: platform.RouteSpec{OperationID: "revokeAPIKey", Method: http.MethodDelete, Pattern: base + "/{key_id}", Access: platform.AdminSession, Scope: "apikeys:write"}, Handler: http.HandlerFunc(h.revoke)},
	}
}
func (h *Handler) target(r *http.Request) (string, string, error) {
	if h.repository == nil {
		return "", "", auth.ErrUnavailable
	}
	tenant, e := platform.AuthorizedTenant(r.Context())
	if e != nil || tenant != r.PathValue("tenant_id") {
		return "", "", apperrors.New(404, "not_found", "Tenant not found")
	}
	actor, e := platform.AuthorizedActor(r.Context())
	if e != nil || actor.Kind != "user" {
		return "", "", apperrors.New(401, "unauthenticated", "Admin session required")
	}
	return tenant, actor.ID, nil
}
func writeError(w http.ResponseWriter, r *http.Request, e error) {
	if errors.Is(e, auth.ErrUnavailable) {
		e = apperrors.New(503, "authentication_unavailable", "")
	}
	if errors.Is(e, auth.ErrNotFound) {
		e = apperrors.New(404, "not_found", "API key not found")
	}
	platform.WriteProblem(w, r, e)
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, user, e := h.target(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	items, e := h.repository.List(r.Context(), tenant, user)
	if e != nil {
		writeError(w, r, e)
		return
	}
	if items == nil {
		items = []contract.APIKeyView{}
	}
	jsonResponse(w, http.StatusOK, struct {
		Items []contract.APIKeyView `json:"items"`
	}{items})
}
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tenant, user, e := h.target(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	var dto struct {
		Scopes    *[]string       `json:"scopes"`
		ExpiresAt json.RawMessage `json:"expires_at"`
	}
	if e := platform.DecodeJSON(w, r, &dto); e != nil {
		writeError(w, r, e)
		return
	}
	invalid := func() { writeError(w, r, apperrors.New(400, "invalid_api_key", "API key input invalid")) }
	if dto.Scopes == nil || len(*dto.Scopes) > 64 {
		invalid()
		return
	}
	allowed := auth.RoleScopes("owner")
	seen := map[string]bool{}
	for _, scope := range *dto.Scopes {
		valid := false
		for _, candidate := range allowed {
			if scope == candidate {
				valid = true
				break
			}
		}
		if !valid || seen[scope] {
			invalid()
			return
		}
		seen[scope] = true
	}
	var actorScopes []string
	if h.scopes != nil {
		actorScopes = h.scopes(r.Context())
	}
	if !slices.Contains(actorScopes, "apikeys:write") {
		writeError(w, r, apperrors.New(403, "scope_forbidden", "Required scope is unavailable"))
		return
	}
	for _, scope := range *dto.Scopes {
		if !slices.Contains(actorScopes, scope) {
			writeError(w, r, apperrors.New(403, "scope_forbidden", "Requested key scope is unavailable"))
			return
		}
	}
	var expires *time.Time
	if dto.ExpiresAt != nil {
		if string(dto.ExpiresAt) == "null" {
			invalid()
			return
		}
		var value time.Time
		if json.Unmarshal(dto.ExpiresAt, &value) != nil || value.IsZero() {
			invalid()
			return
		}
		expires = &value
	}
	var secret [32]byte
	if _, e := rand.Read(secret[:]); e != nil {
		writeError(w, r, auth.ErrUnavailable)
		return
	}
	id := uuid.NewString()
	material := contract.KeyMaterial{ID: id, Prefix: id[:8], Hash: sha256.Sum256(secret[:]), Scopes: append([]string{}, (*dto.Scopes)...), ExpiresAt: expires}
	created, e := h.repository.Create(r.Context(), tenant, user, material)
	if e != nil {
		writeError(w, r, e)
		return
	}
	jsonResponse(w, http.StatusCreated, contract.Created{Key: id + "." + base64.RawURLEncoding.EncodeToString(secret[:]), APIKey: created})
}
func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	tenant, user, e := h.target(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	id := r.PathValue("key_id")
	parsed, e := uuid.Parse(id)
	if e != nil || parsed.String() != id {
		writeError(w, r, apperrors.New(400, "invalid_api_key", "API key ID invalid"))
		return
	}
	if e := h.repository.Revoke(r.Context(), tenant, user, id); e != nil {
		writeError(w, r, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
