// Package store adapts SQLC queries for checkout's private orchestration.
// Dependencies: generated db and caller-owned pgx.Tx; no SQL strings, connections
// or lifecycle operations. Foreign tables are read-only; all errors are stable.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

func UUID(s string) pgtype.UUID { var u pgtype.UUID; _ = u.Scan(s); return u }
func Text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505":
			return contract.ErrKeyReused
		case "23514", "22023", "22P02", "22003":
			return contract.ErrInvalid
		case "42501":
			return contract.ErrForbidden
		case "40001", "40P01":
			return contract.ErrRetry
		case "23503":
			return contract.ErrNotFound
		}
	}
	return contract.ErrUnavailable
}
func Queries(ctx context.Context, tx pgx.Tx, tenantID string) (*db.Queries, error) {
	if tx == nil {
		return nil, contract.ErrUnavailable
	}
	q := db.New(tx)
	t, e := q.GetOrdersTenantContext(ctx)
	if e != nil {
		return nil, MapError(e)
	}
	if !t.Valid || t.String() != tenantID {
		return nil, contract.ErrForbidden
	}
	return q, nil
}
