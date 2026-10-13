// Queue infrastructure owns River's pooled connections and installer bootstrap.
// Dependencies: pgxpool and tenanttx; callers: worker composition and river-migrate.
// Invariants: application pools use fixed approved roles; business effects always
// use InQueueTenant. Bootstrap authority belongs only to the operator installer.
package pgxadapter

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// QueuePool is River's native driver type, exposed only for infrastructure
// composition. It must never be handed to a business module or request handler.
type QueuePool = pgxpool.Pool

func queuePoolConfig(dsn, role string) (*pgxpool.Config, error) {
	switch role {
	case "app_queue_worker", "app_outbox_worker", "app_river_migrator":
	default:
		return nil, errors.New("unsupported queue connection role")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("queue connection configuration failed")
	}
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	return cfg, nil
}

// OpenQueuePool installs the fixed role on every physical connection and pings
// before returning; missing role authority therefore fails at startup.
func OpenQueuePool(ctx context.Context, dsn, role string) (*QueuePool, error) {
	cfg, err := queuePoolConfig(dsn, role)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("queue connection configuration failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("queue connection capability unavailable")
	}
	return pool, nil
}

// OpenMigrationPool is exclusively the River installer bootstrap connection:
// it creates/verifies the NOLOGIN owner before OpenQueuePool switches to it.
// Its connecting authority is supplied by operator configuration, never input.
func OpenMigrationPool(ctx context.Context, dsn string) (*QueuePool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("installer connection configuration failed")
	}
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("installer connection configuration failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("installer connection unavailable")
	}
	return pool, nil
}

// InQueueTenant uses the same checked transaction contract as Runner.InTenant.
// Tenant comes from trusted worker configuration or authenticated admission.
func InQueueTenant(ctx context.Context, pool *QueuePool, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if pool == nil {
		return errors.New("queue pool is nil")
	}
	return tenanttx.Run(ctx, tenant, pool.Begin,
		func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, id)
			return err
		}, fn)
}
