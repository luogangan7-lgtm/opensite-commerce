// Responsibilities: expose ledger posting to application orchestrators.
// Dependencies: pgx transactions and ledger values; callers own transaction
// lifetime and authorization. Post preserves balanced, immutable journal entries.
package contract

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Poster posts inside the caller's authorized transaction; it never commits it.
type Poster interface {
	Post(context.Context, pgx.Tx, Transaction) (string, error)
}
