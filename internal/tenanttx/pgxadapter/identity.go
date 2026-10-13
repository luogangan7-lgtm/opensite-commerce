// Responsibilities: own dedicated identity LOGIN connections and transactions.
// Dependencies: pgx; callers: private identity stores, including the offline CLI.
// Invariants: no raw connection escapes, no tenant context, READ COMMITTED work;
// callbacks must finish synchronously and the caller serializes connection use.
package pgxadapter

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IdentityRunner keeps the dedicated LOGIN private; callers validate its exact
// capabilities through sqlc inside the transaction before performing work.
type IdentityRunner struct{ conn *pgx.Conn }

func OpenIdentity(ctx context.Context, dsn string) (*IdentityRunner, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("identity database configuration failed")
	}
	config.ConnectTimeout = 10 * time.Second
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, errors.New("identity database connection failed")
	}
	return &IdentityRunner{conn: conn}, nil
}

// InMaintenance runs global identity work under the connecting LOGIN, without
// tenant scope. A callback error rolls back; nil commits. Never retain tx.
func (r *IdentityRunner) InMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || r.conn == nil {
		return errors.New("identity database is unavailable")
	}
	if fn == nil {
		return errors.New("identity callback is nil")
	}
	return pgx.BeginTxFunc(ctx, r.conn, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true)`); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func (r *IdentityRunner) Close(ctx context.Context) error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close(ctx)
}
