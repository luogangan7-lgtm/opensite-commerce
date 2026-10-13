// Package notificationhttp adapts the notifications public port to authenticated
// generated HTTP routes. Bootstrap mounts it; business modules never import it.
// Invariants: exact tenant authorization, bounded requests, safe response schemas,
// and queued redrive receipts without asserting SMTP delivery or exactly once.
package notificationhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

type Handler struct {
	generated.StrictServerInterface
	service contract.Service
	schemas openapi3.Schemas
}

func New(service contract.Service) *Handler {
	doc, err := generated.GetSwagger()
	if err != nil {
		panic(err)
	}
	return &Handler{service: service, schemas: doc.Components.Schemas}
}
func (h *Handler) Routes() []httpapi.Route {
	const base = "/api/v1/t/{tenant_id}/notifications"
	routes := httpapi.StrictRoutes(h, []httpapi.RouteSpec{
		{OperationID: "listNotificationFailures", Method: "GET", Pattern: base + "/failures", Access: httpapi.AdminOrAPIKey, Scope: "notifications:write"},
		{OperationID: "redriveNotification", Method: "POST", Pattern: base + "/{notification_id}/redrive", Access: httpapi.AdminOrAPIKey, Scope: "notifications:write"},
	})
	for i := range routes {
		next := routes[i].Handler
		routes[i].Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			// These commands have no request body; one byte suffices to reject even a
			// chunked input without allocating or waiting for an unbounded body.
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) != 0 {
				httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "Request body is not allowed"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return routes
}
func (h *Handler) authorize(ctx context.Context, tenant string) error {
	actual, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return err
	}
	if actual != tenant {
		return apperrors.New(404, "notification_not_found", "Notification not found")
	}
	if h.service == nil {
		return apperrors.New(503, "notification_unavailable", "Notifications are unavailable")
	}
	return nil
}
func (h *Handler) representation(name string, value, destination any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var raw any
	if err = json.Unmarshal(body, &raw); err != nil {
		return err
	}
	if err = h.schemas[name].Value.VisitJSON(raw); err != nil {
		return err
	}
	return json.Unmarshal(body, destination)
}
func (h *Handler) ListNotificationFailures(ctx context.Context, r generated.ListNotificationFailuresRequestObject) (generated.ListNotificationFailuresResponseObject, error) {
	tenant := r.TenantId.String()
	if err := h.authorize(ctx, tenant); err != nil {
		return nil, err
	}
	limit := int32(25)
	if r.Params.Limit != nil {
		limit = *r.Params.Limit
	}
	items, err := h.service.ListFailures(ctx, tenant, limit)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []contract.Failure{}
	}
	var body generated.NotificationFailures
	if err := h.representation("NotificationFailures", struct {
		Items []contract.Failure `json:"items"`
	}{items}, &body); err != nil {
		return nil, err
	}
	cache := "no-store"
	return generated.ListNotificationFailures200JSONResponse{Body: body, Headers: generated.ListNotificationFailures200ResponseHeaders{CacheControl: &cache}}, nil
}
func (h *Handler) RedriveNotification(ctx context.Context, r generated.RedriveNotificationRequestObject) (generated.RedriveNotificationResponseObject, error) {
	tenant := r.TenantId.String()
	if err := h.authorize(ctx, tenant); err != nil {
		return nil, err
	}
	receipt, err := h.service.Redrive(ctx, tenant, r.NotificationId.String(), r.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	var body generated.NotificationRedriveReceipt
	if err := h.representation("NotificationRedriveReceipt", receipt, &body); err != nil {
		return nil, err
	}
	cache := "no-store"
	return generated.RedriveNotification200JSONResponse{Body: body, Headers: generated.RedriveNotification200ResponseHeaders{CacheControl: &cache}}, nil
}
