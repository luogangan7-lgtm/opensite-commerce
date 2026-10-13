// Responsibilities: expose fulfillment commands through compiler-checked HTTP.
// Dependencies: public contract, strict platform routing and an authorized tx port.
// Callers: bootstrap. Invariants: URL tenants never authorize database work;
// quantities/versions/idempotency are command inputs, not stock/payment proof.
package fulfillment

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

type HTTPConfig struct {
	Run     func(context.Context, string, func(context.Context, pgx.Tx) error) error
	Service contract.Service
}
type HTTP struct {
	httpapi.StrictServerInterface
	config HTTPConfig
}

func NewHTTP(config HTTPConfig) *HTTP {
	if config.Service == nil {
		config.Service = New()
	}
	return &HTTP{config: config}
}
func (s *HTTP) Routes() []httpapi.Route {
	specs := []httpapi.RouteSpec{{OperationID: "createShipment", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/orders/{order_id}/shipments", Access: httpapi.AdminOrAPIKey, Scope: "fulfillment:write", MaxBodySize: httpapi.DefaultMaxBodySize}}
	routes := httpapi.StrictRoutes(s, specs)
	for i := range routes {
		next := routes[i].Handler
		routes[i].Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "This command does not accept query parameters"))
				return
			}
			if _, err := idempotency.ValidateKey(r.Header); err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return routes
}
func authorized(ctx context.Context, requested uuid.UUID) (string, error) {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return "", err
	}
	if tenant != requested.String() {
		return "", apperrors.New(404, "not_found", "Resource not found")
	}
	return tenant, nil
}
func version(value string) (int64, error) {
	if value == "" {
		return 0, apperrors.New(428, "precondition_required", "If-Match is required")
	}
	if len(value) >= 3 && value[0] == '"' && value[len(value)-1] == '"' {
		raw := value[1 : len(value)-1]
		n, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && n > 0 && strconv.FormatInt(n, 10) == raw {
			return n, nil
		}
	}
	return 0, apperrors.New(400, "invalid_if_match", "If-Match must be a quoted positive decimal version")
}
func mapHTTPError(err error) error {
	if err == nil {
		return nil
	}
	status, code, detail := 500, "internal_error", "An internal error occurred"
	switch {
	case errors.Is(err, contract.ErrNotFound):
		status, code, detail = 404, "not_found", "Resource not found"
	case errors.Is(err, contract.ErrPrecondition):
		status, code, detail = 412, "precondition_failed", "Order version changed"
	case errors.Is(err, contract.ErrKeyReused):
		status, code, detail = 422, "idempotency_key_reused", "Idempotency key is bound to a different command"
	case errors.Is(err, contract.ErrConflict):
		status, code, detail = 409, "fulfillment_conflict", "Shipment state or quantity conflicts with the order"
	case errors.Is(err, contract.ErrInvalid):
		status, code, detail = 422, "invalid_fulfillment", "Invalid shipment command"
	case errors.Is(err, contract.ErrRetry):
		status, code, detail = 503, "retry_required", "Retry this command with the same idempotency key"
	case errors.Is(err, contract.ErrUnavailable):
		status, code, detail = 503, "fulfillment_unavailable", "Fulfillment service is unavailable"
	}
	return apperrors.New(status, code, detail)
}
func (s *HTTP) CreateShipment(ctx context.Context, r httpapi.CreateShipmentRequestObject) (httpapi.CreateShipmentResponseObject, error) {
	tenant, err := authorized(ctx, r.TenantId)
	if err != nil {
		return nil, err
	}
	v, err := version(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, mapHTTPError(contract.ErrInvalid)
	}
	in := contract.CreateInput{OrderID: r.OrderId.String(), ExpectedVersion: v, IdempotencyKey: r.Params.IdempotencyKey, Carrier: r.Body.Carrier, TrackingNumber: r.Body.TrackingNumber}
	for _, line := range r.Body.Lines {
		in.Lines = append(in.Lines, contract.Line{OrderLineID: line.OrderLineId.String(), Quantity: line.Quantity})
	}
	if err = in.Validate(); err != nil {
		return nil, mapHTTPError(err)
	}
	if s.config.Run == nil {
		return nil, mapHTTPError(contract.ErrUnavailable)
	}
	var shipment contract.Shipment
	err = s.config.Run(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		if current, e := httpapi.AuthorizedTenant(txctx); e != nil || current != tenant {
			return contract.ErrNotFound
		}
		var e error
		shipment, e = s.config.Service.CreateShipment(txctx, tx, tenant, in)
		return e
	})
	if err != nil {
		return nil, mapHTTPError(err)
	}
	result := httpapi.ShipmentRecord{Id: uuid.MustParse(shipment.ID), OrderId: uuid.MustParse(shipment.OrderID), OrderVersion: shipment.OrderVersion, Status: shipment.Status, Lines: []httpapi.ShipmentRecordLine{}}
	for _, line := range shipment.Lines {
		result.Lines = append(result.Lines, httpapi.ShipmentRecordLine{Id: uuid.MustParse(line.ID), OrderLineId: uuid.MustParse(line.OrderLineID), Quantity: line.Quantity})
	}
	return httpapi.CreateShipment201JSONResponse(result), nil
}
