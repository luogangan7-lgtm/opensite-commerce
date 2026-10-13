//go:build !pgx

// Responsibilities: fail closed when durable C-04 database support is absent.
// Dependencies: public command port; callers may inject deterministic test ports.
// Invariant: no process-local idempotency is installed in production.
package httpapi

import (
	"context"
	"github.com/jackc/pgx/v5"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"time"
)

func claimCommandTx(context.Context, pgx.Tx, CommandScope, [32]byte, time.Duration, idempotency.ResponseStore) (CommandCompletion, *idempotency.Response, error) {
	return nil, nil, apperrors.New(503, "idempotency_unavailable", "Durable command persistence requires the pgx build")
}
