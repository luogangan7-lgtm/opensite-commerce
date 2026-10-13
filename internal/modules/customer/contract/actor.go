// Customer actor context is a trusted middleware output, never an input DTO.
// Dependencies: context only; cart/checkout/privacy adapters consume this port.
// Invariant: tenant-bound sessions are checked and verified before publishing it.
package contract

import "context"

type Actor struct{ TenantID, CustomerID, SessionID string }
type actorKey struct{}

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}
