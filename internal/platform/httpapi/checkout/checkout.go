// Package checkout adapts the checkout contract to generated strict HTTP routes.
// Dependencies: checkout/payment contracts, platform HTTP admission; no SQL/pools.
// Caller: bootstrap. The URL is a target, actor comes only from a trusted resolver,
// and Fake raw signatures are admitted only in explicit nonproduction composition.
package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const (
	StartOperation   = "createCheckout"
	PreviewOperation = "previewCheckout"
	GetOperation     = "getCheckout"
	FakeOperation    = "receiveFakePaymentEvent"
	StartPattern     = "/api/v1/t/{tenant_id}/checkout"
	PreviewPattern   = "/api/v1/t/{tenant_id}/checkout/preview"
	GetPattern       = "/api/v1/t/{tenant_id}/checkout/{checkout_id}"
	FakePattern      = "/api/v1/testing/payments/fake/webhook"
	startBodyLimit   = 256 << 10
	fakeBodyLimit    = 64 << 10
)

// StorefrontResolver must verify customer session or opaque guest capability,
// session-bound CSRF, verified host/site and target tenant before constructing an
// actor. It must never use the admin cookie. No production resolver exists here.
type StorefrontResolver interface {
	Resolve(context.Context, *http.Request) (contract.AuthorizedActor, error)
}

// SignatureVerifier authenticates exact raw bytes and performs trusted provider
// account/tenant mapping before producing an event. Core rechecks DB binding.
type SignatureVerifier interface {
	Verify(context.Context, []byte, string) (payment.VerifiedEvent, error)
}
type Config struct {
	Service     contract.Service
	Resolver    StorefrontResolver
	Origin      string
	FakeEnabled bool
	Environment string
	Verifier    SignatureVerifier
}
type Handler struct {
	httpapi.StrictServerInterface
	config Config
}
type actorKey struct{}
type eventKey struct{}
type cartIDKey struct{}

func New(config Config) (*Handler, error) {
	if config.Resolver != nil {
		u, e := url.Parse(config.Origin)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("checkout resolver requires exact HTTPS storefront origin")
		}
	}
	if config.FakeEnabled {
		if config.Environment != "test" && config.Environment != "development" {
			return nil, fmt.Errorf("Fake payment webhook is forbidden outside test/development")
		}
		if config.Verifier == nil {
			return nil, fmt.Errorf("Fake payment webhook requires a signature verifier")
		}
	}
	return &Handler{config: config}, nil
}

// Authenticator handles only the exact checkout RouteSpec; all other operations
// remain subject to the existing authenticator. A configured admin authenticator
// therefore cannot accidentally authorize this storefront capability seam.
func (s *Handler) Authenticator(fallback httpapi.Authenticator) httpapi.Authenticator {
	if fallback == nil {
		fallback = httpapi.DenyAuthenticator{}
	}
	return authFunc(func(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
		if spec.OperationID != StartOperation && spec.OperationID != PreviewOperation && spec.OperationID != GetOperation {
			return fallback.Authenticate(ctx, r, spec)
		}
		valid := (spec.OperationID == StartOperation && spec.Method == http.MethodPost && spec.Pattern == StartPattern) || (spec.OperationID == PreviewOperation && spec.Method == http.MethodPost && spec.Pattern == PreviewPattern) || (spec.OperationID == GetOperation && spec.Method == http.MethodGet && spec.Pattern == GetPattern)
		if spec.Access != httpapi.Storefront || spec.Scope != "" || !valid || s.config.Resolver == nil {
			return nil, unauthenticated()
		}
		origins, tokens := r.Header.Values("Origin"), r.Header.Values("X-CSRF-Token")
		if len(origins) != 1 || origins[0] != s.config.Origin || len(tokens) != 1 || tokens[0] == "" || len(tokens[0]) > 512 {
			return nil, apperrors.New(403, "forbidden", "Storefront Origin and CSRF admission required")
		}
		actor, err := s.config.Resolver.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		if !actor.Valid() {
			return nil, unauthenticated()
		}
		if actor.TenantID() != r.PathValue("tenant_id") {
			return nil, notFound()
		}
		ctx = context.WithValue(ctx, actorKey{}, actor)
		return context.WithValue(ctx, cartIDKey{}, r.Header.Get("X-Cart-ID")), nil
	})
}

type authFunc func(context.Context, *http.Request, httpapi.RouteSpec) (context.Context, error)

func (f authFunc) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	return f(ctx, r, s)
}
func (s *Handler) Routes() []httpapi.Route {
	specs := []httpapi.RouteSpec{{OperationID: StartOperation, Method: http.MethodPost, Pattern: StartPattern, Access: httpapi.Storefront, MaxBodySize: startBodyLimit}}
	if s.config.FakeEnabled {
		specs = append(specs, httpapi.RouteSpec{OperationID: FakeOperation, Method: http.MethodPost, Pattern: FakePattern, Access: httpapi.Public, MaxBodySize: fakeBodyLimit})
	}
	routes := httpapi.StrictRoutes(s, specs)
	for i := range routes {
		if routes[i].Spec.OperationID == FakeOperation {
			routes[i].Handler = s.verifyWebhook(routes[i].Handler)
		}
	}
	routes = append(routes,
		httpapi.Route{Spec: httpapi.RouteSpec{OperationID: PreviewOperation, Method: http.MethodPost, Pattern: PreviewPattern, Access: httpapi.Storefront, MaxBodySize: startBodyLimit}, Handler: http.HandlerFunc(s.preview)},
		httpapi.Route{Spec: httpapi.RouteSpec{OperationID: GetOperation, Method: http.MethodGet, Pattern: GetPattern, Access: httpapi.Storefront}, Handler: http.HandlerFunc(s.get)},
	)
	return routes
}

func (s *Handler) browserReader(r *http.Request) (contract.AuthorizedActor, contract.BrowserReader, error) {
	actor, ok := r.Context().Value(actorKey{}).(contract.AuthorizedActor)
	tenant, err := httpapi.AuthorizedTenant(r.Context())
	if !ok || !actor.Valid() || err != nil {
		return contract.AuthorizedActor{}, nil, unauthenticated()
	}
	if tenant != actor.TenantID() {
		return contract.AuthorizedActor{}, nil, notFound()
	}
	reader, ok := s.config.Service.(contract.BrowserReader)
	if !ok {
		return contract.AuthorizedActor{}, nil, apperrors.New(503, "checkout_unavailable", "Checkout is unavailable")
	}
	return actor, reader, nil
}

type previewOptionJSON struct {
	RateID      string `json:"rate_id"`
	Method      string `json:"method"`
	AmountMinor string `json:"amount_minor"`
}
type previewJSON struct {
	SubtotalMinor   string              `json:"subtotal_minor"`
	TaxMinor        string              `json:"tax_minor"`
	ShippingMinor   string              `json:"shipping_minor"`
	TotalMinor      string              `json:"total_minor"`
	Currency        string              `json:"currency"`
	ShippingOptions []previewOptionJSON `json:"shipping_options"`
}

func (s *Handler) preview(w http.ResponseWriter, r *http.Request) {
	actor, reader, err := s.browserReader(r)
	if err != nil {
		httpapi.WriteProblem(w, r, err)
		return
	}
	var body struct {
		CartID          string `json:"cart_id"`
		ShippingAddress struct {
			Country    string `json:"country"`
			Region     string `json:"region"`
			PostalCode string `json:"postal_code"`
		} `json:"shipping_address"`
	}
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		httpapi.WriteProblem(w, r, err)
		return
	}
	if body.CartID != r.Header.Get("X-Cart-ID") {
		httpapi.WriteProblem(w, r, notFound())
		return
	}
	result, err := reader.Preview(r.Context(), actor, contract.PreviewInput{CartID: body.CartID, Country: body.ShippingAddress.Country, Region: body.ShippingAddress.Region, PostalCode: body.ShippingAddress.PostalCode})
	if err != nil {
		httpapi.WriteProblem(w, r, mapError(err))
		return
	}
	options := make([]previewOptionJSON, 0, len(result.ShippingOptions))
	for _, option := range result.ShippingOptions {
		options = append(options, previewOptionJSON{option.RateID, option.Method, strconv.FormatInt(option.AmountMinor, 10)})
	}
	writeJSON(w, http.StatusOK, previewJSON{strconv.FormatInt(result.SubtotalMinor, 10), strconv.FormatInt(result.TaxMinor, 10), strconv.FormatInt(result.ShippingMinor, 10), strconv.FormatInt(result.TotalMinor, 10), result.Currency, options})
}
func (s *Handler) get(w http.ResponseWriter, r *http.Request) {
	actor, reader, err := s.browserReader(r)
	if err != nil {
		httpapi.WriteProblem(w, r, err)
		return
	}
	result, err := reader.Get(r.Context(), actor, r.PathValue("checkout_id"), r.Header.Get("X-Cart-ID"))
	if err != nil {
		httpapi.WriteProblem(w, r, mapError(err))
		return
	}
	dto, err := representation(result)
	if err != nil {
		httpapi.WriteProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Handler) verifyWebhook(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exact bytes reach the verifier before strict generated JSON decoding. The
		// outer router has already applied middleware and the route's body limit.
		values := r.Header.Values("X-Fake-Signature")
		if len(values) != 1 || values[0] == "" {
			httpapi.WriteProblem(w, r, unauthenticated())
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, fakeBodyLimit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				httpapi.WriteProblem(w, r, apperrors.New(413, "request_too_large", "Request body exceeds the route limit"))
			} else {
				httpapi.WriteProblem(w, r, apperrors.New(400, "invalid_json", "Invalid request body"))
			}
			return
		}
		event, err := s.config.Verifier.Verify(r.Context(), raw, values[0])
		if err != nil || event.Validate() != nil {
			httpapi.WriteProblem(w, r, unauthenticated())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), eventKey{}, event)))
	})
}
func (s *Handler) CreateCheckout(ctx context.Context, r httpapi.CreateCheckoutRequestObject) (httpapi.CreateCheckoutResponseObject, error) {
	actor, ok := ctx.Value(actorKey{}).(contract.AuthorizedActor)
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if !ok || !actor.Valid() || err != nil {
		return nil, unauthenticated()
	}
	if tenant != actor.TenantID() || r.TenantId.String() != tenant {
		return nil, notFound()
	}
	if r.Body == nil {
		return nil, apperrors.New(400, "invalid_json", "Checkout body required")
	}
	if r.Body.QuoteId != nil {
		return nil, apperrors.New(422, "quote_checkout_unsupported", "Quote checkout is not implemented")
	}
	body := r.Body
	if (actor.Kind() == "guest" && body.BuyerContact == nil) || (actor.Kind() == "customer" && body.BuyerContact != nil) {
		return nil, apperrors.New(422, "buyer_contact_invalid", "Guest contact is required only for guest checkout")
	}
	if s.config.Service == nil {
		return nil, apperrors.New(503, "checkout_unavailable", "Checkout is unavailable")
	}
	contact := orders.Contact{}
	if body.BuyerContact != nil {
		contact = orders.Contact{Email: body.BuyerContact.Email, Name: value(body.BuyerContact.Name), Phone: value(body.BuyerContact.Phone)}
	}
	input := contract.StartInput{CartID: body.CartId.String(), IdempotencyKey: string(r.Params.IdempotencyKey), BuyerContact: contact, ShippingAddress: contract.ShippingAddress{Country: body.ShippingAddress.Country, Subdivision: value(body.ShippingAddress.Subdivision), Address1: body.ShippingAddress.Address1, Address2: value(body.ShippingAddress.Address2), City: body.ShippingAddress.City, PostalCode: body.ShippingAddress.PostalCode}}
	if cartID, ok := ctx.Value(cartIDKey{}).(string); ok && cartID != "" && cartID != input.CartID {
		return nil, notFound()
	}
	result, err := s.config.Service.Start(ctx, actor, input)
	if err != nil {
		return nil, mapError(err)
	}
	dto, err := representation(result)
	if err != nil {
		return nil, err
	}
	return httpapi.CreateCheckout201JSONResponse(dto), nil
}
func (s *Handler) ReceiveFakePaymentEvent(ctx context.Context, _ httpapi.ReceiveFakePaymentEventRequestObject) (httpapi.ReceiveFakePaymentEventResponseObject, error) {
	event, ok := ctx.Value(eventKey{}).(payment.VerifiedEvent)
	if !ok || event.Validate() != nil {
		return nil, unauthenticated()
	}
	if !s.config.FakeEnabled || s.config.Service == nil {
		return nil, apperrors.New(503, "checkout_unavailable", "Checkout is unavailable")
	}
	result, err := s.config.Service.Confirm(ctx, event)
	if err != nil {
		return nil, mapError(err)
	}
	dto, err := representation(result)
	if err != nil {
		return nil, err
	}
	return httpapi.ReceiveFakePaymentEvent200JSONResponse(dto), nil
}
func representation(r contract.Result) (httpapi.CheckoutResult, error) {
	checkoutID, e1 := uuid.Parse(r.CheckoutID)
	orderID, e2 := uuid.Parse(r.OrderID)
	if e1 != nil || e2 != nil || checkoutID == uuid.Nil || orderID == uuid.Nil || r.AmountMinor <= 0 || r.OrderVersion <= 0 {
		return httpapi.CheckoutResult{}, fmt.Errorf("invalid checkout result")
	}
	dto := httpapi.CheckoutResult{CheckoutId: checkoutID, OrderId: orderID, State: r.State, AmountMinor: r.AmountMinor, Currency: r.Currency, OrderVersion: r.OrderVersion}
	dto.Payment.ProviderRef = r.Payment.ProviderRef
	dto.Payment.Status = string(r.Payment.Status)
	if r.Payment.NextActionKind != "" {
		dto.Payment.NextActionKind = &r.Payment.NextActionKind
	}
	if r.Payment.NextActionURL != "" {
		dto.Payment.NextActionUrl = &r.Payment.NextActionURL
	}
	return dto, nil
}
func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func unauthenticated() error {
	return apperrors.New(401, "unauthenticated", "Authentication is required")
}
func notFound() error { return apperrors.New(404, "not_found", "Checkout resource not found") }

// mapError covers checkout errors and the Orders errors Start/Confirm return
// unchanged; anything else stays a redacted 500.
func mapError(err error) error {
	switch {
	case errors.Is(err, orders.ErrStockUnavailable), errors.Is(err, orders.ErrReservationInactive):
		return apperrors.New(409, "stock_unavailable", "Requested stock is no longer available")
	case errors.Is(err, orders.ErrInvalid):
		return apperrors.New(422, "validation_failed", "Invalid checkout request")
	case errors.Is(err, orders.ErrKeyReused):
		return apperrors.New(422, "idempotency_key_reused", "Idempotency key already bound to different content")
	case errors.Is(err, orders.ErrNotFound), errors.Is(err, orders.ErrReservationOwner):
		return notFound()
	case errors.Is(err, orders.ErrConflict):
		return apperrors.New(409, "checkout_conflict", "Checkout state conflict")
	case errors.Is(err, orders.ErrRetry), errors.Is(err, orders.ErrUnavailable):
		return apperrors.New(503, "checkout_unavailable", "Checkout is unavailable")
	case errors.Is(err, contract.ErrInvalid):
		return apperrors.New(422, "validation_failed", "Invalid checkout request")
	case errors.Is(err, contract.ErrForbidden), errors.Is(err, contract.ErrNotFound):
		return notFound()
	case errors.Is(err, contract.ErrKeyReused):
		return apperrors.New(422, "idempotency_key_reused", "Idempotency key already bound to different content")
	case errors.Is(err, contract.ErrConflict):
		return apperrors.New(409, "checkout_conflict", "Checkout state conflict")
	case errors.Is(err, contract.ErrUnavailable), errors.Is(err, contract.ErrRetry):
		return apperrors.New(503, "checkout_unavailable", "Checkout is unavailable")
	default:
		return err
	}
}
