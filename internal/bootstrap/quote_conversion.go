// Responsibility: compose quote conversion with the single existing-order checkout.
// Dependencies: public quotes/orders/checkout/customer ports and HTTP admission;
// callers: bootstrap.New and real PostgreSQL gates. No tables or payment IO here.
// Invariants: typed configuration cannot inject a prepayment implementation;
// company comes from a live session's authorized quote, never request fields.
package bootstrap

import (
	"context"
	"errors"
	"net/http"

	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
)

type QuoteConversionConfig struct {
	Runner quotes.TenantRunner
	Quotes qc.ConversionRepository
	Orders oc.QuoteCreator
}

func NewQuoteConversion(config QuoteConversionConfig, service checkoutcontract.Service) (*quotes.ConversionApplication, error) {
	adopter, ok := service.(checkoutcontract.ExistingOrderService)
	if config.Runner == nil || config.Quotes == nil || config.Orders == nil || !ok || adopter == nil {
		return nil, errors.New("quote conversion requires existing-order checkout and tenant persistence")
	}
	return quotes.NewConversionApplication(quotes.ConversionConfig{Runner: config.Runner, Quotes: config.Quotes, Orders: config.Orders, Checkout: quoteCheckout{adopter}}), nil
}

type quoteCheckout struct {
	service checkoutcontract.ExistingOrderService
}

func (p quoteCheckout) Prepay(ctx context.Context, actor qc.AuthorizedActor, receipt qc.Conversion) (qc.Prepayment, error) {
	if !actor.Valid() || actor.Kind() != "customer" || receipt.CustomerID != actor.ID() || receipt.CompanyID != actor.CompanyID() || !qc.ValidID(receipt.OrderID) {
		return qc.Prepayment{}, qc.ErrUnauthorized
	}
	buyer, err := checkoutcontract.NewAuthorizedActor(actor.TenantID(), "customer", actor.ID())
	if err != nil {
		return qc.Prepayment{}, qc.ErrUnauthorized
	}
	// Cart Start shares checkout's actor/key namespace; the immutable receipt
	// order supplies a separate internal key while the quote retains its raw key.
	result, err := p.service.AdoptOrder(ctx, buyer, receipt.OrderID, "quotes.orders.v1:"+receipt.OrderID)
	if err != nil {
		switch {
		case errors.Is(err, checkoutcontract.ErrNotFound):
			return qc.Prepayment{}, qc.ErrNotFound
		case errors.Is(err, checkoutcontract.ErrForbidden):
			return qc.Prepayment{}, qc.ErrForbidden
		case errors.Is(err, checkoutcontract.ErrKeyReused):
			return qc.Prepayment{}, qc.ErrIdempotencyReused
		case errors.Is(err, checkoutcontract.ErrConflict), errors.Is(err, checkoutcontract.ErrRetry):
			return qc.Prepayment{}, qc.ErrConflict
		default:
			return qc.Prepayment{}, qc.ErrUnavailable
		}
	}
	if result.OrderID != receipt.OrderID || result.AmountMinor != receipt.Amount.Amount || result.Currency != receipt.Amount.Currency {
		return qc.Prepayment{}, qc.ErrUnavailable
	}
	state := ""
	switch result.State {
	case "ready", "pending", "creating", "confirming", "cancelling":
		state = string(oc.PendingPayment)
	case "paid":
		state = string(oc.Paid)
	case "cancelled", "failed":
		state = string(oc.Cancelled)
	default:
		return qc.Prepayment{}, qc.ErrUnavailable
	}
	// Conversion re-reads the owned order projection before accepting this state.
	// Capture is reached only through the signed provider HTTP Confirm route.
	return qc.Prepayment{OrderID: result.OrderID, Amount: receipt.Amount, State: state, ProviderRef: result.Payment.ProviderRef}, nil
}

type quoteBuyerAdmission struct {
	customer *customerhttp.Handler
	runner   quotes.TenantRunner
}

func (a quoteBuyerAdmission) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.OperationID != "createQuoteOrder" || spec.Method != http.MethodPost || spec.Pattern != "/api/v1/t/{tenant_id}/quotes/{quote_id}/orders" || spec.Access != httpapi.BuyerCompany || spec.Scope != "quotes:convert" {
		return nil, apperrors.New(401, "unauthenticated", "Customer authentication is required")
	}
	actor, err := a.customer.Resolve(ctx, r)
	if err != nil {
		return nil, err
	}
	if actor.CustomerID == "" {
		return nil, apperrors.New(401, "unauthenticated", "Customer authentication is required")
	}
	ctx = customercontract.WithActor(ctx, actor)
	identity, err := quotes.ResolveBuyerForQuote(ctx, a.runner, actor.TenantID, actor.CustomerID, r.PathValue("quote_id"))
	if err != nil {
		if errors.Is(err, qc.ErrNotFound) || errors.Is(err, qc.ErrInvalid) {
			return nil, apperrors.New(404, "not_found", "Resource not found")
		}
		return nil, apperrors.New(503, "quote_authorization_unavailable", "Quote authorization is unavailable")
	}
	return qc.WithTrustedActor(ctx, identity), nil
}
