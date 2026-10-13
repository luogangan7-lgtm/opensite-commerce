//go:build pgx

// Responsibilities: serialize PostgreSQL test fixture initialization across packages.
// Dependencies: pgxadapter maintenance transactions; only tagged test fixtures call here.
// Invariants: one fixed session lock in the COMMON maintenance database protects
// migrations and cluster role DDL; business/concurrency tests run after release.
package pgxtest

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const fixtureInitializationLockKey int64 = 18004518

// WithFixtureInitialization executes one fixture's migration and role DDL phase.
// run must be the private maintenance adapter of a *_test.go caller, connected to
// the COMMON source database, never its new fixture database. The caller closes
// that adapter on every outcome: session locks survive transaction rollback.
// Keeping connection ownership in test files preserves the maintenance-call policy.
func WithFixtureInitialization(ctx context.Context, run func(context.Context, func(context.Context, pgx.Tx) error) error, initialize func(context.Context) error) error {
	if initialize == nil || run == nil {
		return errors.New("fixture initialization callback missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return run(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_lock($1)", fixtureInitializationLockKey); err != nil {
			return err
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			var released bool
			if unlockErr := tx.QueryRow(cleanup, "SELECT pg_advisory_unlock($1)", fixtureInitializationLockKey).Scan(&released); unlockErr != nil || !released {
				err = errors.Join(err, errors.New("fixture initialization advisory unlock failed"))
			}
		}()
		return initialize(ctx)
	})
}
