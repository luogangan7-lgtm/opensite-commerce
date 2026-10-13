// Responsibilities: own dedicated identity LOGIN connections and transactions.
// Dependencies: pgx/pgxpool; callers: private identity/tenant stores and offline CLI.
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
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

// IdentityRunner keeps the dedicated LOGIN private. Authentication work uses
// its role-admitted query port; maintenance callers validate their capabilities.
type IdentityRunner struct {
	pool             *pgxpool.Pool
	operationTimeout time.Duration
}

func OpenIdentity(ctx context.Context, dsn string) (*IdentityRunner, error) {
	return openIdentity(ctx, dsn, 1, 0)
}

// OpenAuthorization isolates concurrent tenant authority from the serialized
// identity lane. Its fixed budget covers startup, acquisition, callback and commit.
func OpenAuthorization(ctx context.Context, dsn string) (*IdentityRunner, error) {
	return openIdentity(ctx, dsn, 4, 2*time.Second)
}

// OpenStaffAuthentication keeps staff credential work serialized, with the same
// bounded connection and transaction policy as tenant authority work.
func OpenStaffAuthentication(ctx context.Context, dsn string) (*IdentityRunner, error) {
	return openIdentity(ctx, dsn, 1, 2*time.Second)
}

func openIdentity(ctx context.Context, dsn string, maxConns int32, operationTimeout time.Duration) (*IdentityRunner, error) {
	if operationTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, operationTimeout)
		defer cancel()
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("identity database configuration failed")
	}
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	config.MaxConns, config.MinConns = maxConns, 0
	if operationTimeout > 0 {
		config.ConnConfig.ConnectTimeout = operationTimeout
		if config.ConnConfig.RuntimeParams == nil {
			config.ConnConfig.RuntimeParams = make(map[string]string)
		}
		config.ConnConfig.RuntimeParams["lock_timeout"] = "500ms"
		config.ConnConfig.RuntimeParams["statement_timeout"] = "1500ms"
	}
	// A dead idle connection must be replaced before any authentication work.
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("identity database connection failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("identity database connection failed")
	}
	return &IdentityRunner{pool: pool, operationTimeout: operationTimeout}, nil
}

// InMaintenance runs global identity work under the connecting LOGIN, without
// tenant scope. A callback error rolls back; nil commits. Never retain tx.
func (r *IdentityRunner) InMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return r.inMaintenance(ctx, fn)
}

// RunAuthentication exposes only generated queries after checking the exact
// dedicated app_auth LOGIN inside this transaction. Pre-admission identity
// lookups have no tenant; admitted callers still set and revalidate their
// private tenant stamp before any tenant work. Never retain the queries.
func (r *IdentityRunner) RunAuthentication(ctx context.Context, fn func(context.Context, *db.Queries) error) error {
	if fn == nil {
		return errors.New("authentication callback is nil")
	}
	return r.inMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		valid, err := q.ValidateAuthConnection(ctx)
		if err != nil || !valid.Valid || !valid.Bool {
			return errors.New("authentication database is unavailable")
		}
		return fn(ctx, q)
	})
}

func (r *IdentityRunner) inMaintenance(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || r.pool == nil {
		return errors.New("identity database is unavailable")
	}
	if fn == nil {
		return errors.New("identity callback is nil")
	}
	if r.operationTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.operationTimeout)
		defer cancel()
	}
	return pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true)`); err != nil {
			return err
		}
		if r.operationTimeout > 0 {
			// Reapply locally so a reused or replaced physical session cannot
			// inherit relaxed defaults; rollback restores the pool's baseline.
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout','500ms',true), set_config('statement_timeout','1500ms',true)`); err != nil {
				return err
			}
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
