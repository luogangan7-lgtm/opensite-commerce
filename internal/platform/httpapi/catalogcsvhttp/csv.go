// Responsibilities: expose typed, bounded catalog CSV routes using its public
// port. Dependencies: generated OAS + shared command transaction admission;
// bootstrap calls Routes. Only an authorized subject reaches the caller runner.
package catalogcsvhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"io"
	"net/http"
)

type Config struct {
	httpapi.CommandConfig
	Service contract.CSVPort
}
type Handler struct {
	httpapi.StrictServerInterface
	config Config
}

func New(c Config) *Handler { return &Handler{config: c} }

const base = "/api/v1/t/{tenant_id}/catalog/csv"

func (h *Handler) Routes() []httpapi.Route {
	routes := httpapi.StrictRoutes(h, []httpapi.RouteSpec{
		{OperationID: "preflightCatalogCSV", Method: "POST", Pattern: base + "/preflight", Access: httpapi.AdminOrAPIKey, Scope: "catalog:import", MaxBodySize: contract.CSVMaxBytes},
		{OperationID: "importCatalogCSV", Method: "POST", Pattern: base + "/import", Access: httpapi.AdminOrAPIKey, Scope: "catalog:import", MaxBodySize: contract.CSVMaxBytes},
		{OperationID: "exportCatalogCSV", Method: "GET", Pattern: base + "/export", Access: httpapi.AdminOrAPIKey, Scope: "catalog:export"},
	})
	for i := range routes {
		if routes[i].Spec.OperationID == "importCatalogCSV" {
			routes[i] = httpapi.AdmitCommand(routes[i])
		}
	}
	return routes
}
func csvError(err error) error {
	switch {
	case errors.Is(err, contract.ErrCSVInvalid):
		return apperrors.New(422, "invalid_csv", "CSV validation failed")
	case errors.Is(err, contract.ErrCSVLimit):
		return apperrors.New(413, "csv_limit", "CSV exceeds the byte or row limit")
	case errors.Is(err, contract.ErrCSVUnsupported):
		return apperrors.New(422, "csv_unsupported", "CSV cannot represent this catalog")
	}
	return err
}
func reportView(report contract.CSVReport) (httpapi.CatalogCSVReport, error) {
	if report.Errors == nil {
		report.Errors = []contract.CSVRowError{}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return httpapi.CatalogCSVReport{}, err
	}
	var result httpapi.CatalogCSVReport
	err = json.Unmarshal(raw, &result)
	return result, err
}
func (h *Handler) PreflightCatalogCSV(ctx context.Context, r httpapi.PreflightCatalogCSVRequestObject) (httpapi.PreflightCatalogCSVResponseObject, error) {
	tenant := r.TenantId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Run == nil || h.config.Service == nil {
		return nil, httpapi.Unavailable()
	}
	var report contract.CSVReport
	err := h.config.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		report, err = h.config.Service.Preflight(ctx, tx, r.Body)
		return err
	})
	if err != nil {
		return nil, csvError(err)
	}
	view, err := reportView(report)
	return httpapi.PreflightCatalogCSV200JSONResponse(view), err
}

type commandResponse struct{ idempotency.Response }

func (r commandResponse) VisitImportCatalogCSVResponse(w http.ResponseWriter) error {
	return httpapi.WriteResponse(w, r.Response)
}
func (h *Handler) ImportCatalogCSV(ctx context.Context, r httpapi.ImportCatalogCSVRequestObject) (httpapi.ImportCatalogCSVResponseObject, error) {
	tenant := r.TenantId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Service == nil {
		return nil, httpapi.Unavailable()
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, contract.CSVMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > contract.CSVMaxBytes {
		return nil, csvError(contract.ErrCSVLimit)
	}
	response, err := httpapi.Command(ctx, h.config.CommandConfig, tenant, "importCatalogCSV", base+"/import", map[string]string{"tenant_id": tenant}, func(ctx context.Context, tx pgx.Tx, _ httpapi.Subject) (idempotency.Response, error) {
		return importResponse(h.config.Service.Import(ctx, tx, bytes.NewReader(raw)))
	})
	return commandResponse{response}, err
}

// importResponse stores row diagnostics as a 200 report. Structural failures
// (bad header, encoding, empty file) carry no row errors and must stay 422,
// matching preflight, rather than replaying an empty "successful" report.
func importResponse(report contract.CSVReport, err error) (idempotency.Response, error) {
	if err != nil && !(errors.Is(err, contract.ErrCSVInvalid) && len(report.Errors) > 0) {
		return idempotency.Response{}, csvError(err)
	}
	view, err := reportView(report)
	if err != nil {
		return idempotency.Response{}, err
	}
	return httpapi.JSONResponse(200, view)
}
func (h *Handler) ExportCatalogCSV(ctx context.Context, r httpapi.ExportCatalogCSVRequestObject) (httpapi.ExportCatalogCSVResponseObject, error) {
	tenant := r.TenantId.String()
	if _, err := httpapi.RequireSubject(ctx, tenant, h.config.Subject); err != nil {
		return nil, err
	}
	if h.config.Run == nil || h.config.Service == nil {
		return nil, httpapi.Unavailable()
	}
	var body []byte
	err := h.config.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		body, err = h.config.Service.Export(ctx, tx)
		return err
	})
	if err != nil {
		return nil, csvError(err)
	}
	return httpapi.ExportCatalogCSV200TextcsvResponse{Body: bytes.NewReader(body), ContentLength: int64(len(body))}, nil
}
