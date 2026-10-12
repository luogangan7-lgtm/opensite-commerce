//go:build pgx

// Responsibilities: open a configured runtime-role pool and run authorized sqlc
// work through tenanttx. Dependencies: pgxpool, platform stamp, sqlc; callers:
// bootstrap only opens it, modules receive the checked runner, never the pool.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type QueryRunner struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*QueryRunner, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, errors.New("runtime database configuration is invalid")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("runtime database is unavailable")
	}
	return &QueryRunner{pool: pool}, nil
}
func (r *QueryRunner) Close() {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
}
func (r *QueryRunner) RunAuthorized(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	if r == nil || r.pool == nil {
		return errors.New("runtime database is unavailable")
	}
	if fn == nil {
		return errors.New("query callback is nil")
	}
	return (Runner{Pool: r.pool}).InTenant(ctx, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(db.New(tx)) })
}
