// Responsibilities: seal an admitted write-authority recheck into request context.
// Dependencies: sqlc query port; Router stamps it and tenanttx runs it inside the
// business transaction. Invariant: no request field can construct this stamp,
// stale stamps are cleared, and a failed recheck prevents domain work/commit.
package httpapi

import (
	"context"
	"net/http"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type TransactionAuthorizationResolver interface {
	ResolveTransactionAuthorization(context.Context, *http.Request, RouteSpec) (func(context.Context, *db.Queries) error, error)
}
type transactionAuthorizationKey struct{}

// RevalidateTransaction executes the authenticator's sealed recheck using the
// caller's transaction. Existing non-staff capability authenticators need no
// staff authority check; production staff always supplies one for tenant writes.
func RevalidateTransaction(ctx context.Context, q *db.Queries) error {
	if check, ok := ctx.Value(transactionAuthorizationKey{}).(func(context.Context, *db.Queries) error); ok && check != nil {
		return check(ctx, q)
	}
	return nil
}
func isWrite(method string) bool { return method != "GET" && method != "HEAD" && method != "OPTIONS" }
