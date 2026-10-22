// Package checkout adapts the checkout contract to generated strict HTTP routes.
// Dependencies: checkout/payments contracts, platform HTTP admission; no SQL/pools.
// Caller: bootstrap. The URL is a target, actor comes only from a trusted resolver,
// and raw Fake events reach Payments only after strict nonproduction admission.
package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	contract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	payments "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

const (
	StartOperation = "createCheckout"
	FakeOperation  = "receiveFakePaymentEvent"
	StartPattern   = "/api/v1/t/{tenant_id}/checkout"
	FakePattern    = "/api/v1/testing/payments/fake/webhook"
	startBodyLimit = 256 << 10
	fakeBodyLimit  = 64 << 10
)

// StorefrontResolver must verify customer session or opaque guest capability,
// session-bound CSRF, verified host/site and target tenant before constructing an
// actor. It must never use the admin cookie. No production resolver exists here.
type StorefrontResolver interface {
	Resolve(context.Context, *http.Request) (contract.AuthorizedActor, error)
}

// SignatureVerifier is the retired compatibility port.
// Deprecated: it cannot authorize a webhook; use payments.Receiver.
type SignatureVerifier interface {
	Verify(context.Context, []byte, string) (payment.VerifiedEvent, error)
}
type Config struct {
	Service     contract.Service
	Resolver    StorefrontResolver
	Origin      string
	FakeEnabled bool
	Environment string
	Events      payments.Receiver
	// Deprecated: ignored; an Events receiver is required for Fake admission.
	Verifier SignatureVerifier
}
type Handler struct {
	httpapi.StrictServerInterface
	config Config
}
type actorKey struct{}
type rawEventKey struct{}
type rawEvent struct {
	body      []byte
	signature string
}

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
		if config.Events == nil {
			return nil, fmt.Errorf("Fake payment webhook requires a payments receiver")
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
	return composedAuth{fallback: fallback, authenticate: authFunc(func(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
		if spec.OperationID != StartOperation {
			return fallback.Authenticate(ctx, r, spec)
		}
		if spec.Access != httpapi.Storefront || spec.Scope != "" || spec.Method != http.MethodPost || spec.Pattern != StartPattern || s.config.Resolver == nil {
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
		return context.WithValue(ctx, actorKey{}, actor), nil
	})}
}

// Preserve the staff projection and transaction guard through this wrapper.
type composedAuth struct {
	fallback     httpapi.Authenticator
	authenticate authFunc
}

func (a composedAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	return a.authenticate(ctx, r, spec)
}
func (a composedAuth) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	if spec.OperationID == StartOperation {
		return httpapi.Actor{}, nil
	}
	if resolver, ok := a.fallback.(httpapi.ActorResolver); ok {
		return resolver.ResolveActor(ctx, r, spec)
	}
	return httpapi.Actor{}, nil
}
func (a composedAuth) ResolveTransactionAuthorization(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	if spec.OperationID == StartOperation {
		return nil, nil
	}
	if resolver, ok := a.fallback.(httpapi.TransactionAuthorizationResolver); ok {
		return resolver.ResolveTransactionAuthorization(ctx, r, spec)
	}
	return nil, nil
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
			routes[i].Handler = s.captureWebhook(routes[i].Handler)
		}
	}
	return routes
}
func (s *Handler) captureWebhook(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preserve exact bytes before strict JSON decoding. No receiver or domain
		// write runs until StrictRoutes accepts the body and signature header. The
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
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), rawEventKey{}, rawEvent{body: raw, signature: values[0]})))
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
func (s *Handler) ReceiveFakePaymentEvent(ctx context.Context, r httpapi.ReceiveFakePaymentEventRequestObject) (httpapi.ReceiveFakePaymentEventResponseObject, error) {
	raw, ok := ctx.Value(rawEventKey{}).(rawEvent)
	if !ok || r.Body == nil {
		return nil, unauthenticated()
	}
	if !s.config.FakeEnabled || s.config.Events == nil {
		return nil, apperrors.New(503, "payments_unavailable", "Payments are unavailable")
	}
	receipt, err := s.config.Events.Receive(ctx, raw.body, raw.signature)
	if err != nil {
		return nil, err
	}
	if receipt.Status == "rejected" {
		return nil, apperrors.New(409, "payment_conflict", "Payment event conflict")
	}
	if receipt.EventID == "" || (receipt.Status != "pending" && receipt.Status != "processed") {
		return nil, fmt.Errorf("invalid payment event receipt")
	}
	return httpapi.ReceiveFakePaymentEvent200JSONResponse{EventId: receipt.EventID, Status: httpapi.FakePaymentReceiptStatus(receipt.Status)}, nil
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
