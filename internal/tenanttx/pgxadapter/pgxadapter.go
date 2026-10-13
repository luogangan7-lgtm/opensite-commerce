//go:build pgx

// Package pgxadapter connects the checked tenanttx runner with pgxpool.
// This file is compiled in the PostgreSQL-enabled CI job (-tags pgx).
// Invariant: this is the only package that holds a pgxpool or opens raw pgx
// connections (scripts/check_modules.py); callers only ever receive a pgx.Tx.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// Runner hands out transactions; the pool is unexported so callers cannot
// bypass tenant scoping by acquiring connections directly.
type Runner struct{ pool *pgxpool.Pool }

// Open builds a pool from dsn and pings it. close releases the pool; call it
// once, after every InTenant/InMaintenance call has returned.
func Open(ctx context.Context, dsn string) (Runner, func(), error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return Runner{}, nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return Runner{}, nil, err
	}
	return Runner{pool: pool}, pool.Close, nil
}

// InTenant requires the caller to authenticate and authorize tenantID first.
// fn receives ONLY a transaction, not the pool. sqlc's db.New(tx) should be
// constructed inside fn. Never return tx or use it asynchronously.
func (r Runner) InTenant(ctx context.Context, tenantID string, fn func(context.Context, pgx.Tx) error) error {
	return r.inTenant(ctx, tenantID, pgx.TxOptions{}, fn)
}

// InTenantSnapshot is InTenant in one REPEATABLE READ, READ ONLY transaction,
// so multi-statement reads observe a single snapshot and cannot write.
func (r Runner) InTenantSnapshot(ctx context.Context, tenantID string, fn func(context.Context, pgx.Tx) error) error {
	return r.inTenant(ctx, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (r Runner) inTenant(ctx context.Context, tenantID string, options pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	if r.pool == nil {
		return errors.New("pgx pool is nil")
	}
	return tenanttx.Run(ctx, tenantID,
		func(ctx context.Context) (pgx.Tx, error) { return r.pool.BeginTx(ctx, options) },
		func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, id)
			return err
		}, fn,
	)
}

// InMaintenance runs fn in a transaction with NO app.tenant_id, under the
// connecting role. It is for tests and operator tooling (seeding fixtures,
// asserting database-level rejections) only; request paths must use InTenant.
// fn's error rolls back; nil commits. Same tx rules as InTenant.
func (r Runner) InMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return r.inMaintenance(ctx, fn)
}

// inMaintenance is private adapter initialization machinery, never a request port.
func (r Runner) inMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if r.pool == nil {
		return errors.New("pgx pool is nil")
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}
