// Responsibility: tenant-scoped staff read of retained open refund cases.
// Dependencies: generated transport/SQL and authorized snapshot runner; caller:
// bootstrap. Invariants: existing refund scope, no provider payload or money writes.
package payments

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

type ReconciliationHTTP struct {
	generated.StrictServerInterface
	read func(context.Context, string, func(*db.Queries) error) error
}

func NewReconciliationHTTP(read func(context.Context, string, func(*db.Queries) error) error) *ReconciliationHTTP {
	return &ReconciliationHTTP{read: read}
}

func (h *ReconciliationHTTP) Routes() []httpapi.Route {
	return httpapi.StrictRoutes(h, []httpapi.RouteSpec{{OperationID: "listRefundCases", Method: http.MethodGet,
		Pattern: "/api/v1/t/{tenant_id}/payments/refund-cases", Access: httpapi.AdminOrAPIKey, Scope: "returns:write"}})
}

func (h *ReconciliationHTTP) ListRefundCases(ctx context.Context, in generated.ListRefundCasesRequestObject) (generated.ListRefundCasesResponseObject, error) {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return nil, err
	}
	if tenant != in.TenantId.String() {
		return nil, apperrors.New(404, "not_found", "Resource not found")
	}
	if h.read == nil {
		return nil, apperrors.New(503, "payments_unavailable", "Reconciliation persistence unavailable")
	}
	var raw []byte
	err = h.read(ctx, tenant, func(q *db.Queries) error {
		readOnly, err := q.GetOrdersTransactionReadOnly(ctx)
		if err != nil {
			return err
		}
		if !readOnly {
			return apperrors.New(503, "payments_unavailable", "Reconciliation persistence unavailable")
		}
		after := id("")
		if in.Params.AfterId != nil {
			after = id(in.Params.AfterId.String())
		}
		raw, err = q.OpenRefundCases(ctx, after)
		return err
	})
	if err != nil {
		return nil, apperrors.New(503, "payments_unavailable", "Reconciliation persistence unavailable")
	}
	var metrics struct {
		Count int64 `json:"open_count"`
		Age   int64 `json:"oldest_age_seconds"`
	}
	if err = json.Unmarshal(raw, &metrics); err != nil {
		return nil, apperrors.New(503, "payments_unavailable", "Reconciliation persistence unavailable")
	}
	slog.InfoContext(ctx, "payment_refund_cases_open", "count", metrics.Count, "oldest_age_seconds", metrics.Age)
	return refundCasesResponse(raw), nil
}

type refundCasesResponse []byte

func (out refundCasesResponse) VisitListRefundCasesResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, err := w.Write(out)
	return err
}
