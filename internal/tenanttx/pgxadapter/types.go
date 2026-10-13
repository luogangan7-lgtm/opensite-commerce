// Package pgxadapter exposes only inert PostgreSQL result and error types here.
// Dependencies: pgconn types; callers: generated-query consumers and domain error
// mappers. Invariant: these aliases convey no connector, pool or connection access.
package pgxadapter

import "github.com/jackc/pgx/v5/pgconn"

// CommandTag is the result of a completed SQL command, with no connection methods.
type CommandTag = pgconn.CommandTag

// PgError preserves PostgreSQL error matching without exposing pgconn connectors.
type PgError = pgconn.PgError
