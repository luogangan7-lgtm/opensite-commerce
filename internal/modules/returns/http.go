// Responsibilities: expose return commands through strict generated transport.
// Dependencies: public return/payment contracts, authorization and bounded routing.
// Callers: bootstrap. Invariants: refunds accept no client PSP/account/amount proof;
// the trusted service owns its committed preparation and completion transactions.
package returns

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

type HTTPConfig struct {
	Run         func(context.Context, string, func(context.Context, pgx.Tx) error) error
	Service     contract.Service
	Idempotency httpapi.CommandConfig
}
type HTTP struct {
	httpapi.StrictServerInterface
	config HTTPConfig
}

func NewHTTP(config HTTPConfig) *HTTP {
	if config.Idempotency.Run == nil {
		config.Idempotency.Run = config.Run
	}
	if config.Service == nil {
		config.Service = New(Config{})
	}
	return &HTTP{config: config}
}
func (s *HTTP) Routes() []httpapi.Route {
	specs := []httpapi.RouteSpec{
		{OperationID: "createReturn", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/orders/{order_id}/returns"},
		{OperationID: "receiveReturn", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/returns/{return_id}/receive"},
		{OperationID: "refundReturn", Method: http.MethodPost, Pattern: "/api/v1/t/{tenant_id}/returns/{return_id}/refund"},
	}
	for i := range specs {
		specs[i].Access = httpapi.AdminOrAPIKey
		specs[i].Scope = "returns:write"
		specs[i].MaxBodySize = httpapi.DefaultMaxBodySize
	}
	routes := httpapi.StrictRoutes(s, specs)
	for i := range routes {
		next := httpapi.CaptureCommand(routes[i].Spec, routes[i].Handler)
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
	if _, err := httpapi.AuthorizedActor(ctx); err != nil {
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
		status, code, detail = 409, "return_conflict", "Return state or quantity conflicts with the order"
	case errors.Is(err, contract.ErrInvalid):
		status, code, detail = 422, "invalid_return", "Invalid return command"
	case errors.Is(err, contract.ErrRetry):
		status, code, detail = 503, "retry_required", "Retry this command with the same idempotency key"
	case errors.Is(err, contract.ErrUnavailable):
		status, code, detail = 503, "returns_unavailable", "Return service is unavailable"
	default:
		var provider *payment.PaymentError
		if errors.As(err, &provider) {
			status, code, detail = 503, "payment_unavailable", "Payment service is unavailable"
			switch provider.Code {
			case payment.ErrorNonRetryable, payment.ErrorAmountMismatch, payment.ErrorStateConflict, payment.ErrorIdempotencyConflict:
				status, code, detail = 409, "payment_conflict", "Payment service rejected this refund"
			}
		}
	}
	return apperrors.New(status, code, detail)
}
func (s *HTTP) transaction(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if s.config.Run == nil {
		return contract.ErrUnavailable
	}
	actor, err := httpapi.AuthorizedActor(ctx)
	if err != nil {
		return err
	}
	return s.config.Run(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		if current, e := httpapi.AuthorizedTenant(txctx); e != nil || current != tenant {
			return contract.ErrNotFound
		}
		if current, e := httpapi.AuthorizedActor(txctx); e != nil || current != actor {
			return contract.ErrNotFound
		}
		return fn(txctx, tx)
	})
}
func returnDTO(value contract.Return) httpapi.ReturnRecord {
	result := httpapi.ReturnRecord{Id: uuid.MustParse(value.ID), OrderId: uuid.MustParse(value.OrderID), OrderVersion: value.OrderVersion, Status: value.Status, Kind: httpapi.ReturnRecordKind(value.Kind), RefundMinor: strconv.FormatInt(value.RefundMinor, 10), Restocked: value.Restocked, Lines: []httpapi.ReturnRecordLine{}}
	for _, line := range value.Lines {
		item := httpapi.ReturnRecordLine{Id: uuid.MustParse(line.ID), OrderLineId: uuid.MustParse(line.OrderLineID), Quantity: line.Quantity}
		if line.ShipmentLineID != "" {
			id := uuid.MustParse(line.ShipmentLineID)
			item.ShipmentLineId = &id
		}
		result.Lines = append(result.Lines, item)
	}
	return result
}
func (s *HTTP) CreateReturn(ctx context.Context, r httpapi.CreateReturnRequestObject) (httpapi.CreateReturnResponseObject, error) {
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
	in := contract.RequestInput{OrderID: r.OrderId.String(), ExpectedVersion: v, IdempotencyKey: r.Params.IdempotencyKey, Kind: contract.Kind(r.Body.Kind), Reason: r.Body.Reason}
	for _, line := range r.Body.Lines {
		item := contract.Line{OrderLineID: line.OrderLineId.String(), Quantity: line.Quantity}
		if line.ShipmentLineId != nil {
			item.ShipmentLineID = line.ShipmentLineId.String()
		}
		in.Lines = append(in.Lines, item)
	}
	if err = in.Validate(); err != nil {
		return nil, mapHTTPError(err)
	}
	session, replay, err := httpapi.BeginCommand(ctx, s.config.Idempotency, "createReturn")
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return commandResponse{*replay}, nil
	}
	in.IdempotencyKey = session.Key()
	var completionErr error
	err = s.transaction(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		result, e := s.config.Service.Request(txctx, tx, tenant, in)
		if e != nil {
			return e
		}
		completionErr = session.Complete(txctx, tx, http.StatusCreated, returnDTO(result))
		return completionErr
	})
	if completionErr != nil {
		return nil, completionErr
	}
	if err != nil {
		return nil, mapHTTPError(err)
	}
	return commandResponse{session.Response()}, nil
}
func (s *HTTP) ReceiveReturn(ctx context.Context, r httpapi.ReceiveReturnRequestObject) (httpapi.ReceiveReturnResponseObject, error) {
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
	in := contract.ReceiveInput{ReturnID: r.ReturnId.String(), ExpectedVersion: v, IdempotencyKey: r.Params.IdempotencyKey, Restock: r.Body.Restock}
	if err = in.Validate(); err != nil {
		return nil, mapHTTPError(err)
	}
	session, replay, err := httpapi.BeginCommand(ctx, s.config.Idempotency, "receiveReturn")
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return commandResponse{*replay}, nil
	}
	in.IdempotencyKey = session.Key()
	var completionErr error
	err = s.transaction(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		result, e := s.config.Service.Receive(txctx, tx, tenant, in)
		if e != nil {
			return e
		}
		completionErr = session.Complete(txctx, tx, http.StatusOK, returnDTO(result))
		return completionErr
	})
	if completionErr != nil {
		return nil, completionErr
	}
	if err != nil {
		return nil, mapHTTPError(err)
	}
	return commandResponse{session.Response()}, nil
}
func (s *HTTP) RefundReturn(ctx context.Context, r httpapi.RefundReturnRequestObject) (httpapi.RefundReturnResponseObject, error) {
	tenant, err := authorized(ctx, r.TenantId)
	if err != nil {
		return nil, err
	}
	v, err := version(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if r.Body == nil || len(*r.Body) != 0 {
		return nil, mapHTTPError(contract.ErrInvalid)
	}
	in := contract.RefundInput{ReturnID: r.ReturnId.String(), ExpectedVersion: v, IdempotencyKey: r.Params.IdempotencyKey}
	if err = in.Validate(); err != nil {
		return nil, mapHTTPError(err)
	}
	session, replay, err := httpapi.BeginCommand(ctx, s.config.Idempotency, "refundReturn")
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return commandResponse{*replay}, nil
	}
	completer, ok := s.config.Service.(contract.RefundCompleter)
	if !ok {
		return nil, mapHTTPError(contract.ErrUnavailable)
	}
	in.IdempotencyKey = session.Key()
	var completionErr error
	_, err = completer.RefundWithCompletion(ctx, tenant, in, func(txctx context.Context, tx pgx.Tx, result contract.Return) error {
		completionErr = session.Complete(txctx, tx, http.StatusOK, returnDTO(result))
		return completionErr
	})
	if completionErr != nil {
		return nil, completionErr
	}
	if err != nil {
		return nil, mapHTTPError(err)
	}
	if session.Response().Status != http.StatusOK {
		return nil, mapHTTPError(contract.ErrUnavailable)
	}
	return commandResponse{session.Response()}, nil
}

type commandResponse struct{ response idempotency.Response }

func (r commandResponse) VisitCreateReturnResponse(w http.ResponseWriter) error {
	return httpapi.WriteCommandResponse(w, r.response)
}
func (r commandResponse) VisitReceiveReturnResponse(w http.ResponseWriter) error {
	return httpapi.WriteCommandResponse(w, r.response)
}
func (r commandResponse) VisitRefundReturnResponse(w http.ResponseWriter) error {
	return httpapi.WriteCommandResponse(w, r.response)
}
