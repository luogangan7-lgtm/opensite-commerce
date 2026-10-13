//go:build pgx

// Responsibilities: coordinate PostgreSQL test fixture initialization across packages.
// Dependencies: caller-owned context; only pgx-tagged test fixtures call this helper.
// Invariant: initialization must be serialized in the common maintenance database,
// while business and concurrency assertions run after this helper returns.
package pgxtest

import "context"

const fixtureInitializationLockKey int64 = 18004518

// WithFixtureInitialization executes one fixture's migration and role DDL phase.
// maintenanceDSN must identify the common database, never the new fixture database.
// This unlocked test-support baseline is replaced after its regression turns CI red.
func WithFixtureInitialization(ctx context.Context, maintenanceDSN string, initialize func(context.Context) error) error {
	return initialize(ctx)
}
