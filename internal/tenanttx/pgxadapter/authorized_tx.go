//go:build pgx

// Responsibilities: expose an authorized transaction for domain commands that
// persist idempotency and River jobs atomically. Dependencies: tenant admission
// and Runner; bootstrap injects this seam. The pool never leaves the adapter.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (r *QueryRunner) RunAuthorizedTx(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	if r == nil {
		return errors.New("runtime database is unavailable")
	}
	if fn == nil {
		return errors.New("transaction callback is nil")
	}
	return r.runner.InTenant(ctx, tenant, fn)
}
