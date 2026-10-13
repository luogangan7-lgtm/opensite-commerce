//go:build !pgx

// Responsibilities: keep mutation persistence closed in no-database builds.
// Dependencies: shared HTTP response; bootstrap uses the same typed contract.
package httpapi

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"time"
)

func executeCommand(context.Context, pgx.Tx, idempotency.Scope, [32]byte, time.Duration, idempotency.ResponseStore, func(context.Context, pgx.Tx) (idempotency.Response, error)) (idempotency.Response, error) {
	return idempotency.Response{}, Unavailable()
}
