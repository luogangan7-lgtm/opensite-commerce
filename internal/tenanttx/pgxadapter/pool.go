// Responsibility: provide the single raw-pool factory at the database boundary.
// Dependencies: pgxpool; callers: API composition and database integration tests.
// Invariants: pool construction does not authorize queries; callers pass transaction
// capabilities to business modules and use a distinct application name for lock probes.
package pgxadapter

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the adapter's connection pool, also usable as a transaction capability.
type Pool = pgxpool.Pool

// NewPool parses the connection configuration and optionally identifies its sessions.
func NewPool(ctx context.Context, dsn, applicationName string) (*Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if applicationName != "" {
		config.ConnConfig.RuntimeParams["application_name"] = applicationName
	}
	return pgxpool.NewWithConfig(ctx, config)
}
