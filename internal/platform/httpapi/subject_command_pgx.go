//go:build pgx

// Responsibilities: call the existing transaction-bound idempotency DAO.
// Dependencies: idempotency; command adapters never open a connection here.
package httpapi

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"time"
)

func executeSubjectCommand(ctx context.Context, tx pgx.Tx, scope idempotency.Scope, hash [32]byte, ttl time.Duration, store idempotency.ResponseStore, fn func(context.Context, pgx.Tx) (idempotency.Response, error)) (idempotency.Response, error) {
	return idempotency.ExecuteTx(ctx, tx, scope, hash, ttl, store, fn)
}
