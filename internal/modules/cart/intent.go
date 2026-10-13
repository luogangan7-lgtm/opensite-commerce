// Responsibility: expose a frozen cart-intent consumer without owning a pool.
// Dependencies: public cart contract and private store; callers: checkout static
// composition. Invariants: supplied transaction, row lock, no price projection.
package cart

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/internal/store"
)

func ReadIntent(ctx context.Context, tx pgx.Tx, actor contract.AuthorizedActor, id string) (contract.Intent, error) {
	return store.ReadIntent(ctx, tx, actor, id)
}
