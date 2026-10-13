//go:build pgx

// Responsibilities: open a configured runtime-role pool via Open and run
// authorized sqlc work through Runner.InTenant. Dependencies: Open/Runner,
// platform stamp, sqlc; callers: bootstrap only opens it, modules receive the
// checked runner, never the pool. Errors never echo the DSN.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type QueryRunner struct {
	runner Runner
	close  func()
}

func OpenQueryRunner(ctx context.Context, url string) (*QueryRunner, error) {
	runner, closePool, err := Open(ctx, url)
	if err != nil {
		return nil, errors.New("runtime database is unavailable or misconfigured")
	}
	return &QueryRunner{runner: runner, close: closePool}, nil
}
func (r *QueryRunner) Close() {
	if r != nil && r.close != nil {
		r.close()
		r.close = nil
	}
}
func (r *QueryRunner) RunAuthorized(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	return r.run(ctx, tenant, false, fn)
}

// RunAuthorizedSnapshot is RunAuthorized in a read-only single-snapshot transaction.
func (r *QueryRunner) RunAuthorizedSnapshot(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	return r.run(ctx, tenant, true, fn)
}

// RunAuthorizedTx preserves the private admission context for a command's
// domain changes and its C-04 response completion in the same transaction.
func (r *QueryRunner) RunAuthorizedTx(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	if r == nil || fn == nil {
		return errors.New("runtime database is unavailable")
	}
	return r.runner.InTenant(ctx, tenant, fn)
}

func (r *QueryRunner) RunAuthorizedTxSnapshot(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	return r.RunCapabilitySnapshot(ctx, tenant, fn)
}

// RunCapabilitySnapshot is only composed behind the CMS module's verified
// signed-preview capability. It always uses a read-only snapshot and tenant
// RLS; route parameters alone must never authorize calling this port.
func (r *QueryRunner) RunCapabilitySnapshot(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || fn == nil {
		return errors.New("runtime database is unavailable")
	}
	return r.runner.InTenantSnapshot(ctx, tenant, fn)
}

// RunTrustedTx is only composed behind the storefront serving refresher, whose tenants
// come from trusted deployment configuration (never a request). It is an ordinary tenant
// write transaction under runtime RLS; request paths must use RunAuthorizedTx.
func (r *QueryRunner) RunTrustedTx(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if r == nil || fn == nil {
		return errors.New("runtime database is unavailable")
	}
	return r.runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { // REVIEW PROBE 96-r1b
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true)`); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func (r *QueryRunner) run(ctx context.Context, tenant string, snapshot bool, fn func(*db.Queries) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	if r == nil {
		return errors.New("runtime database is unavailable")
	}
	if fn == nil {
		return errors.New("query callback is nil")
	}
	in := r.runner.InTenant
	if snapshot {
		in = r.runner.InTenantSnapshot
	}
	return in(ctx, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(db.New(tx)) })
}
