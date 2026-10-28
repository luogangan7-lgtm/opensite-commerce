// Responsibility: dormant owner step-up route and exact receipt-only route shape.
// Dependencies: sealed staff admission, shared login KDF, platform strict JSON.
// Invariants: no registration by default; session + Origin/CSRF + explicit owner;
// no secret in logs/replays, no application-clock proof deadlines.
package httpapi

import (
	"net/http"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func merchantStepUpSpec() platform.RouteSpec {
	return platform.RouteSpec{OperationID: "merchantDeletionStepUp", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/merchant-deletion/step-up", Access: platform.AdminSession, Scope: "tenant:delete", MaxBodySize: 4096}
}

func merchantReceiptRoute(spec platform.RouteSpec) bool {
	return spec.OperationID == "getMerchantDeletion" && spec.Method == "GET" && spec.Pattern == "/api/v1/t/{tenant_id}/merchant-deletion/{request_id}" && spec.Access == platform.AdminSession && spec.Scope == "tenant:delete"
}

// MerchantStepUpRoute is composed explicitly by the activation card. Routes()
// deliberately excludes it, so this card creates no reachable deletion API.
func (h *Handler) MerchantStepUpRoute() platform.Route {
	return platform.Route{Spec: merchantStepUpSpec(), Handler: http.HandlerFunc(h.merchantStepUp)}
}

func (h *Handler) merchantStepUp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := h.admitted(r.Context(), r, merchantStepUpSpec())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if p.Kind != "session" || h.authorization == nil || h.service == nil {
		writeError(w, r, scopeDenied())
		return
	}
	member, err := h.authorization.Membership(r.Context(), p.UserID, p.TenantID)
	if err != nil || member.UserID != p.UserID || member.TenantID != p.TenantID || member.Role != "owner" {
		writeError(w, r, scopeDenied())
		return
	}
	// Repeat the browser-bound check for direct invocation as well as Router.
	token, ok := cookieValue(r, SessionCookie)
	if !ok || !h.sameOrigin(r) || !h.service.VerifySessionCSRF(token, oneHeader(r, "X-CSRF-Token")) {
		writeError(w, r, apperrors.New(403, "csrf_invalid", "Request authenticity could not be verified"))
		return
	}
	var input struct {
		Password string `json:"password"`
		Action   string `json:"action"`
	}
	if err := platform.DecodeJSON(w, r, &input); err != nil {
		writeError(w, r, err)
		return
	}
	actor, ok := login.ActorFromContext(r.Context())
	if !ok {
		writeError(w, r, unauthenticated())
		return
	}
	ip, err := h.clientIP(r)
	if err != nil {
		writeError(w, r, unauthenticated())
		return
	}
	device, ok := cookieValue(r, DeviceCookie)
	if !ok {
		writeError(w, r, unauthenticated())
		return
	}
	proof, err := h.service.MerchantStepUp(r.Context(), actor, p.TenantID, input.Action, input.Password, device, ip)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proof)
}
