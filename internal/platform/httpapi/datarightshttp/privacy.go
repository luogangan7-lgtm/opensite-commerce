// Responsibilities: typed privacy commands/status/downloads using the public
// data-rights port. Dependencies: generated OAS and authorized command runner;
// bootstrap mounts these routes. Wire data grants neither actor nor tenant rights.
package datarightshttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"net/http"
	"net/url"
)

type Config struct {
	httpapi.CommandConfig
	Service contract.Service
	Enqueue func(context.Context, pgx.Tx, string) error
}
type Handler struct {
	httpapi.StrictServerInterface
	config Config
}

func New(c Config) *Handler { return &Handler{config: c} }

const base = "/api/v1/t/{tenant_id}/customers/{customer_id}"

func (h *Handler) Routes() []httpapi.Route {
	routes := httpapi.StrictRoutes(h, []httpapi.RouteSpec{
		{OperationID: "requestCustomerDataExport", Method: "POST", Pattern: base + "/data-exports", Access: httpapi.AdminOrAPIKey, Scope: "customers:privacy"},
		{OperationID: "getCustomerDataExport", Method: "GET", Pattern: base + "/data-exports/{request_id}", Access: httpapi.AdminOrAPIKey, Scope: "customers:privacy"},
		{OperationID: "downloadCustomerDataExport", Method: "GET", Pattern: base + "/data-exports/{request_id}/download", MaxResponseSize: contract.MaxBytes, Access: httpapi.AdminOrAPIKey, Scope: "customers:privacy"},
		{OperationID: "eraseCustomerData", Method: "POST", Pattern: base + "/data-erasure", Access: httpapi.AdminOrAPIKey, Scope: "customers:privacy"},
	})
	for i := range routes {
		if routes[i].Spec.Method == "POST" {
			routes[i] = httpapi.AdmitCommand(routes[i])
		} else {
			next := routes[i].Handler
			routes[i].Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				next.ServeHTTP(w, r)
			})
		}
	}
	return routes
}
func privacyError(err error) error {
	switch {
	case errors.Is(err, contract.ErrInvalid):
		return apperrors.New(400, "invalid_request", "Invalid data-rights input")
	case errors.Is(err, contract.ErrNotFound):
		return apperrors.New(404, "not_found", "Data-rights resource not found")
	case errors.Is(err, contract.ErrConflict):
		return apperrors.New(409, "capability_unavailable", "Export capability unavailable")
	case errors.Is(err, contract.ErrUnavailable):
		return httpapi.Unavailable()
	}
	return err
}

type commandResponse struct{ idempotency.Response }

func (r commandResponse) VisitRequestCustomerDataExportResponse(w http.ResponseWriter) error {
	return httpapi.WriteResponse(w, r.Response)
}
func (r commandResponse) VisitEraseCustomerDataResponse(w http.ResponseWriter) error {
	return httpapi.WriteResponse(w, r.Response)
}
func (h *Handler) RequestCustomerDataExport(ctx context.Context, r httpapi.RequestCustomerDataExportRequestObject) (httpapi.RequestCustomerDataExportResponseObject, error) {
	tenant, customer := r.TenantId.String(), r.CustomerId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Service == nil || h.config.Enqueue == nil {
		return nil, httpapi.Unavailable()
	}
	response, err := httpapi.Command(ctx, h.config.CommandConfig, tenant, "requestCustomerDataExport", base+"/data-exports", map[string]string{"tenant_id": tenant, "customer_id": customer}, func(ctx context.Context, tx pgx.Tx, subject httpapi.Subject) (idempotency.Response, error) {
		request, err := h.config.Service.RequestExport(ctx, tx, tenant, contract.RequestInput{ID: uuid.NewString(), CustomerID: customer, ActorKind: subject.Kind, ActorID: subject.ID})
		if err != nil {
			return idempotency.Response{}, privacyError(err)
		}
		if err = h.config.Enqueue(ctx, tx, request.ID); err != nil {
			return idempotency.Response{}, err
		}
		// Export POST persists only metadata; a raw capability can never enter replay.
		id, err := uuid.Parse(request.ID)
		if err != nil {
			return idempotency.Response{}, err
		}
		return httpapi.JSONResponse(202, httpapi.DataExportRequest{RequestId: id, Status: request.Status})
	})
	return commandResponse{response}, err
}
func (h *Handler) GetCustomerDataExport(ctx context.Context, r httpapi.GetCustomerDataExportRequestObject) (httpapi.GetCustomerDataExportResponseObject, error) {
	tenant := r.TenantId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Service == nil || h.config.Run == nil {
		return nil, httpapi.Unavailable()
	}
	var request contract.Request
	err := h.config.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		request, err = h.config.Service.Status(ctx, tx, tenant, r.RequestId.String(), r.CustomerId.String())
		return err
	})
	if err != nil {
		return nil, privacyError(err)
	}
	view := httpapi.DataExportStatus{RequestId: r.RequestId, Status: request.Status, ExpiresAt: request.ExpiresAt}
	if request.DownloadToken != "" {
		link := "/api/v1/t/" + tenant + "/customers/" + r.CustomerId.String() + "/data-exports/" + r.RequestId.String() + "/download?token=" + url.QueryEscape(request.DownloadToken)
		view.DownloadUrl = &link
		view.DownloadExpiresAt = &request.DownloadExpiresAt
	}
	return httpapi.GetCustomerDataExport200JSONResponse(view), nil
}
func (h *Handler) DownloadCustomerDataExport(ctx context.Context, r httpapi.DownloadCustomerDataExportRequestObject) (httpapi.DownloadCustomerDataExportResponseObject, error) {
	tenant := r.TenantId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Service == nil {
		return nil, httpapi.Unavailable()
	}
	body, err := h.config.Service.Download(ctx, tenant, r.RequestId.String(), r.CustomerId.String(), r.Params.Token)
	if err != nil {
		return nil, privacyError(err)
	}
	return httpapi.DownloadCustomerDataExport200ApplicationoctetStreamResponse{Body: bytes.NewReader(body), ContentLength: int64(len(body))}, nil
}

type erasedView struct {
	Counts   map[string]int `json:"counts"`
	Requests []string       `json:"revoked_exports"`
}

func (h *Handler) EraseCustomerData(ctx context.Context, r httpapi.EraseCustomerDataRequestObject) (httpapi.EraseCustomerDataResponseObject, error) {
	tenant, customer := r.TenantId.String(), r.CustomerId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Service == nil {
		return nil, httpapi.Unavailable()
	}
	response, err := httpapi.Command(ctx, h.config.CommandConfig, tenant, "eraseCustomerData", base+"/data-erasure", map[string]string{"tenant_id": tenant, "customer_id": customer}, func(ctx context.Context, tx pgx.Tx, subject httpapi.Subject) (idempotency.Response, error) {
		result, err := h.config.Service.EraseTx(ctx, tx, tenant, contract.EraseInput{CustomerID: customer, ActorKind: subject.Kind, ActorID: subject.ID})
		if err != nil {
			return idempotency.Response{}, privacyError(err)
		}
		return httpapi.JSONResponse(200, erasedView{Counts: result.Counts, Requests: result.Requests})
	})
	if err != nil {
		return nil, err
	}
	// Persisted opaque request IDs permit an exact replay to retry post-commit
	// cleanup. Revoked DB capabilities already deny access before physical removal.
	var view erasedView
	if err = json.Unmarshal(response.Body, &view); err != nil {
		return nil, err
	}
	if err = h.config.Service.Cleanup(ctx, tenant, view.Requests); err != nil {
		return nil, httpapi.Unavailable()
	}
	return commandResponse{response}, nil
}
