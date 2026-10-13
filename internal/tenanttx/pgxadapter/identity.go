// Responsibilities: own dedicated identity LOGIN connections and transactions.
// Dependencies: pgx/pgxpool; callers: private identity stores, including the offline CLI.
// Invariants: no raw connection or pool escapes, no tenant context, READ COMMITTED
// work; callbacks finish synchronously. One pooled connection is acquired per
// transaction and liveness-checked first, so a terminated backend is replaced
// on the next call instead of failing every later login.
package pgxadapter

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IdentityRunner keeps the dedicated LOGIN private; callers validate its exact
// capabilities through sqlc inside the transaction before performing work.
type IdentityRunner struct{ pool *pgxpool.Pool }

func OpenIdentity(ctx context.Context, dsn string) (*IdentityRunner, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("identity database configuration failed")
	}
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	config.MaxConns, config.MinConns = 1, 0
	// Ping on every acquire: auth traffic is low-volume and already pays for
	// Argon2, while a dead idle connection must never reach a login.
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("identity database connection failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("identity database connection failed")
	}
	return &IdentityRunner{pool: pool}, nil
}

// InMaintenance runs global identity work under the connecting LOGIN, without
// tenant scope. A callback error rolls back; nil commits. Never retain tx.
func (r *IdentityRunner) InMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || r.pool == nil {
		return errors.New("identity database is unavailable")
	}
	if fn == nil {
		return errors.New("identity callback is nil")
	}
	return pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true)`); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func (r *IdentityRunner) Close(ctx context.Context) error {
	if r == nil || r.pool == nil {
		return nil
	}
	r.pool.Close()
	return nil
}
