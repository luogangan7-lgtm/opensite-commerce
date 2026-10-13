//go:build !pgx

// Responsibilities: keep no-database builds usable and configured DB fail-closed.
// Dependencies: context/sqlc checked seam; callers: bootstrap runtime wiring.
// Invariant: no fabricated runner or silently discarded database configuration.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type QueryRunner struct{}

func OpenQueryRunner(context.Context, string) (*QueryRunner, error) {
	return nil, errors.New("database configuration requires the pgx build tag")
}
func (*QueryRunner) Close() {}
func (r *QueryRunner) RunAuthorizedSnapshot(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	return r.RunAuthorized(ctx, tenant, fn)
}
func (*QueryRunner) RunAuthorized(ctx context.Context, tenant string, _ func(*db.Queries) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	return errors.New("runtime database is unavailable")
}

func (*QueryRunner) RunAuthorizedTx(ctx context.Context, tenant string, _ func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	return errors.New("runtime database is unavailable")
}
