// Responsibility: compose customer checkout admission without changing checkout business logic.
// Dependencies: customer/cart/checkout public ports and HTTP admission; caller: bootstrap.New.
// Invariants: one live session lookup, exact checkout route, signed CSRF, and staff context forwarding.
package bootstrap

import (
	"context"
	"net/http"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
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
}

func (a checkoutAdmission) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if a.customer != nil && spec.OperationID == checkouthttp.StartOperation && spec.Access == httpapi.Storefront && spec.Scope == "" && spec.Method == http.MethodPost && spec.Pattern == checkouthttp.StartPattern {
		// The resolver port returns only an actor, so establish the live customer
		// context before checkout intercepts the route. Never reuse a prior stamp.
		actor, err := a.customer.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		if actor.CustomerID == "" {
			return nil, checkoutCustomerRequired()
		}
		ctx = customercontract.WithActor(ctx, actor)
		// Reuse cart's customer/tenant admission and mixed guest-token rejection;
		// this performs no second customer session lookup or cart business call.
		ctx, err = a.cart.Authenticate(ctx, r, spec)
		if err != nil {
			return nil, err
		}
	}
	return a.next.Authenticate(ctx, r, spec)
}

// Preserve the context returned by the selected authenticator when projecting
// staff identity. Storefront and absent staff projections remain empty actors.
func (a checkoutAdmission) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	return a.actors.ResolveActor(ctx, r, spec)
}

type customerCheckoutResolver struct{}

func (customerCheckoutResolver) Resolve(ctx context.Context, _ *http.Request) (checkoutcontract.AuthorizedActor, error) {
	actor, ok := customercontract.ActorFromContext(ctx)
	if !ok || actor.CustomerID == "" {
		return checkoutcontract.AuthorizedActor{}, checkoutCustomerRequired()
	}
	resolved, err := checkoutcontract.NewAuthorizedActor(actor.TenantID, "customer", actor.CustomerID)
	if err != nil {
		return checkoutcontract.AuthorizedActor{}, checkoutCustomerRequired()
	}
	return resolved, nil
}

func checkoutCustomerRequired() error {
	return apperrors.New(http.StatusUnauthorized, "unauthenticated", "Customer authentication is required")
}
