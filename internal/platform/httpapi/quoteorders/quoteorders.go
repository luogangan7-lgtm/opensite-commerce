// Responsibilities: bind quote conversion routes to the generated strict server.
// Dependencies: quotes contract and platform HTTP; bootstrap calls Routes.
// Invariants: trusted buyer/staff stamps precede domain calls; no client money,
// SQL, authentication IO or transaction orchestration belongs to this adapter.
package quoteorders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Service interface {
	Convert(context.Context, string, string) (qc.Conversion, error)
	GetConversion(context.Context, string) (qc.Conversion, error)
}
type Handler struct {
	httpapi.StrictServerInterface
	service Service
}

// New consumes only identities sealed by the router's BuyerCompany resolver.
func New(s Service) *Handler { return &Handler{service: s} }
func (h *Handler) Routes() []httpapi.Route {
	fail := func(w http.ResponseWriter, r *http.Request, e error) { httpapi.WriteProblem(w, r, conversionError(e)) }
	strict := httpapi.NewStrictHandlerWithOptions(h, nil, httpapi.StrictHTTPServerOptions{RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) { fail(w, r, invalidJSON()) }, ResponseErrorHandlerFunc: fail})
	wrapper := httpapi.ServerInterfaceWrapper{Handler: strict, ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
		fail(w, r, apperrors.New(400, "invalid_request", "Invalid request parameter"))
	}}
	create := httpapi.RouteSpec{OperationID: "createQuoteOrder", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}/orders", Access: httpapi.BuyerCompany, Scope: "quotes:convert", MaxBodySize: 1024}
	get := httpapi.RouteSpec{OperationID: "getQuoteOrderConversion", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}/order-conversion", Access: httpapi.AdminOrAPIKey, Scope: "quotes:read"}
	return []httpapi.Route{
		{Spec: create, Handler: h.admit(create, wrapper.CreateQuoteOrder)},
		{Spec: get, Handler: h.admit(get, wrapper.GetQuoteOrderConversion)},
	}
}

func (h *Handler) admit(spec httpapi.RouteSpec, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		var err error
		kind := "staff"
		if spec.OperationID == "createQuoteOrder" {
			kind = "customer"
		}
		if err == nil && ctx == nil {
			err = qc.ErrUnauthorized
		}
		var actor qc.AuthorizedActor
		if err == nil {
			actor, err = qc.ActorFromContext(ctx)
			if err == nil && actor.Kind() != kind {
				err = qc.ErrUnauthorized
			}
		}
		if err == nil && (!qc.ValidID(r.PathValue("tenant_id")) || !qc.ValidID(r.PathValue("quote_id"))) {
			err = apperrors.New(400, "invalid_request", "Identifiers must be canonical nonzero UUIDs")
		}
		if err == nil && actor.TenantID() != r.PathValue("tenant_id") {
			err = qc.ErrNotFound
		}
		if err != nil {
			httpapi.WriteProblem(w, r, conversionError(err))
			return
		}
		r = r.WithContext(ctx)
		if spec.Method == http.MethodPost {
			for _, name := range []string{"Origin", "X-CSRF-Token"} {
				values := r.Header.Values(name)
				if len(values) != 1 || len(strings.TrimSpace(values[0])) == 0 || len(values[0]) > 512 {
					httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_request", "One Origin and session-bound X-CSRF-Token are required"))
					return
				}
			}
			keys := r.Header.Values("Idempotency-Key")
			if len(keys) != 1 || len(keys[0]) < 1 || len(keys[0]) > 128 || strings.IndexFunc(keys[0], func(c rune) bool { return c < 0x21 || c > 0x7e }) >= 0 {
				httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_idempotency_key", "One visible ASCII Idempotency-Key of at most 128 bytes is required"))
				return
			}
			// Decode into a closed object before the generated map DTO. Shared
			// DecodeJSON also rejects duplicates, null, arrays and trailing data.
			var body bytes.Buffer
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(r.Body, &body), r.Body}
			if err = httpapi.DecodeJSON(w, r, &struct{}{}); err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		}
		if h.service == nil {
			httpapi.WriteProblem(w, r, conversionError(qc.ErrUnavailable))
			return
		}
		next(w, r)
	})
}

func (h *Handler) CreateQuoteOrder(ctx context.Context, r httpapi.CreateQuoteOrderRequestObject) (httpapi.CreateQuoteOrderResponseObject, error) {
	v, err := h.service.Convert(ctx, r.QuoteId.String(), r.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	dto, err := representation(v)
	if err != nil {
		return nil, err
	}
	return httpapi.CreateQuoteOrder201JSONResponse(dto), nil
}
func (h *Handler) GetQuoteOrderConversion(ctx context.Context, r httpapi.GetQuoteOrderConversionRequestObject) (httpapi.GetQuoteOrderConversionResponseObject, error) {
	v, err := h.service.GetConversion(ctx, r.QuoteId.String())
	if err != nil {
		return nil, err
	}
	dto, err := representation(v)
	if err != nil {
		return nil, err
	}
	return httpapi.GetQuoteOrderConversion200JSONResponse(dto), nil
}
func representation(v qc.Conversion) (httpapi.QuoteOrderConversion, error) {
	var dto httpapi.QuoteOrderConversion
	// Deliberately map only the public receipt: domain actor IDs and the
	// idempotency key are never serialized, including seller reads.
	value := map[string]any{"quote_id": v.QuoteID, "quote_version": strconv.FormatInt(v.QuoteVersion, 10), "order_id": v.OrderID, "order_number": strconv.FormatInt(v.OrderNumber, 10), "amount": strconv.FormatInt(v.Amount.Amount, 10), "currency": v.Amount.Currency, "state": v.State, "created_at": v.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")}
	if v.ProviderRef != "" {
		value["provider_ref"] = v.ProviderRef
	}
	doc, err := httpapi.GetSwagger()
	if err != nil {
		return dto, err
	}
	if err = doc.Components.Schemas["QuoteOrderConversion"].Value.VisitJSON(value); err != nil {
		return dto, qc.ErrUnavailable
	}
	body, err := json.Marshal(value)
	if err != nil {
		return dto, err
	}
	err = json.Unmarshal(body, &dto)
	return dto, err
}
func invalidJSON() error {
	return apperrors.New(400, "invalid_json", "A single empty JSON object is required")
}
func conversionError(err error) error {
	for _, x := range []struct {
		sentinel error
		status   int
		code     string
	}{{qc.ErrUnauthorized, 401, "unauthenticated"}, {qc.ErrForbidden, 403, "forbidden"}, {qc.ErrNotFound, 404, "not_found"}, {qc.ErrConflict, 409, "quote_conflict"}, {qc.ErrIdempotencyReused, 422, "idempotency_key_reused"}, {qc.ErrInvalid, 422, "validation_failed"}, {qc.ErrOverflow, 422, "amount_overflow"}, {qc.ErrUnavailable, 500, "internal_error"}} {
		if errors.Is(err, x.sentinel) {
			return apperrors.New(x.status, x.code, http.StatusText(x.status))
		}
	}
	return err
}
