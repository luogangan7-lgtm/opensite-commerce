//go:build pgx

// Package pgxadapter connects the checked tenanttx runner with pgxpool.
// This file is compiled in the PostgreSQL-enabled CI job (-tags pgx).
package pgxadapter

import (
	"context"
	"errors"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Runner struct{ Pool *pgxpool.Pool }

// InTenant requires the caller to authenticate and authorize tenantID first.
// fn receives ONLY a transaction, not the pool. sqlc's db.New(tx) should be
// constructed inside fn. Never return tx or use it asynchronously.
func (r Runner) InTenant(ctx context.Context, tenantID string, fn func(context.Context, pgx.Tx) error) error {
	if r.Pool == nil {
		return errors.New("pgx pool is nil")
	}
	return tenanttx.Run(ctx, tenantID,
		r.Pool.Begin,
		func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, id)
			return err
		}, fn,
	)
}
