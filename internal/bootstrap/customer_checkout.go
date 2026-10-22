// Responsibility: compose customer checkout admission without changing checkout business logic.
// Dependencies: customer/cart/checkout public ports and HTTP admission; caller: bootstrap.New.
// Invariants: one live session lookup, exact checkout route, signed CSRF, and staff identity/write authority forwarding.
package bootstrap

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cartcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
)

type checkoutAdmission struct {
	next     httpapi.Authenticator
	actors   httpapi.ActorResolver
	customer *customerhttp.Handler
	cart     *cart.Module
	guest    bool
}

// Non-storefront admission selects the same staff authority as actor projection.
// Checkout never emits or renews an employee credential for customer routes.
func (a checkoutAdmission) AuthenticationResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, spec httpapi.RouteSpec) {
	if spec.Access != httpapi.Storefront {
		if responder, ok := a.actors.(httpapi.AuthenticationResponder); ok {
			responder.AuthenticationResponse(ctx, w, r, spec)
		}
	}
}

func (a checkoutAdmission) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.customer != nil && checkouthttp.BrowserRoute(spec) {
		if a.guest && !hasCheckoutCustomerCookie(r) {
			return a.next.Authenticate(ctx, r, spec)
		}
		// The resolver port returns only an actor, so establish the live customer
		// context before checkout intercepts the route. Never reuse a prior stamp.
		actor, err := a.customer.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		if actor.CustomerID == "" {
			if a.guest {
				return nil, apperrors.New(404, "not_found", "Checkout resource not found")
			}
			if len(r.Header.Values("X-Cart-Token")) == 0 {
				return nil, checkoutCustomerRequired()
			}
			if err := a.customer.AdmitGuest(r); err != nil {
				return nil, err
			}
		} else {
			ctx = customercontract.WithActor(ctx, actor)
		}
		// Reuse cart's customer/tenant admission and mixed guest-token rejection;
		// this performs no second customer session lookup or cart business call.
		ctx, err = a.cart.Authenticate(ctx, r, spec)
		if err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, checkoutCustomerProof{}, true)
	}
	return a.next.Authenticate(ctx, r, spec)
}

// Preserve the context returned by the selected authenticator when projecting
// staff identity. Storefront and absent staff projections remain empty actors.
func (a checkoutAdmission) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	return a.actors.ResolveActor(ctx, r, spec)
}

// Preserve the selected authenticator's sealed write recheck through the outer
// customer wrapper. The downstream authenticator keeps storefront scope separate.
func (a checkoutAdmission) ResolveTransactionAuthorization(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	if resolver, ok := a.next.(httpapi.TransactionAuthorizationResolver); ok {
		return resolver.ResolveTransactionAuthorization(ctx, r, spec)
	}
	return nil, nil
}

type customerCheckoutResolver struct{}

func (customerCheckoutResolver) Resolve(ctx context.Context, _ *http.Request) (checkoutcontract.AuthorizedActor, error) {
	actor, ok := cartcontract.Actor(ctx)
	if !ok {
		return checkoutcontract.AuthorizedActor{}, checkoutCustomerRequired()
	}
	kind, id := "customer", actor.CustomerID
	if id == "" && len(actor.GuestTokenHash) == 32 {
		kind, id = "guest", hex.EncodeToString(actor.GuestTokenHash)
	}
	resolved, err := checkoutcontract.NewAuthorizedActor(actor.TenantID, kind, id)
	if err != nil {
		return checkoutcontract.AuthorizedActor{}, checkoutCustomerRequired()
	}
	return resolved, nil
}

func checkoutCustomerRequired() error {
	return apperrors.New(http.StatusUnauthorized, "unauthenticated", "Customer authentication is required")
}

// The concrete guest/customer composition requires a fresh private customer proof.
type checkoutCustomerProof struct{}
type customerGuestCheckoutResolver struct{ guest *checkouthttp.GuestResolver }

func (g customerGuestCheckoutResolver) Resolve(ctx context.Context, r *http.Request) (checkoutcontract.AuthorizedActor, error) {
	if ok, _ := ctx.Value(checkoutCustomerProof{}).(bool); ok {
		return (customerCheckoutResolver{}).Resolve(ctx, r)
	}
	if hasCheckoutCustomerCookie(r) {
		return checkoutcontract.AuthorizedActor{}, checkoutCustomerRequired()
	}
	return g.guest.Resolve(ctx, r)
}
func hasCheckoutCustomerCookie(r *http.Request) bool {
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == customerhttp.SessionCookie {
				return true
			}
		}
	}
	return false
}
