// Package store fences generated privacy queries with the caller's trusted
// pgx.Tx tenant context. Depends on db/sqlc, never owns a pool or connection.
// Service is the only caller; driver/schema errors never leak to HTTP clients.
package store

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func UUID(s string) (pgtype.UUID, error) {
	u, e := uuid.Parse(s)
	if e != nil || u == uuid.Nil || u.String() != s {
		return pgtype.UUID{}, contract.ErrInvalid
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}
func Tenant(ctx context.Context, tx pgx.Tx, tenant string) (*db.Queries, error) {
	if tx == nil {
		return nil, contract.ErrInvalid
	}
	u, e := UUID(tenant)
	if e != nil {
		return nil, e
	}
	q := db.New(tx).WithTx(tx)
	actual, e := q.DataRightsTenantContext(ctx)
	if e != nil {
		return nil, MapError(e)
	}
	if !actual.Valid || actual.Bytes != u.Bytes {
		return nil, contract.ErrNotFound
	}
	return q, nil
}
func MapError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var p *pgxadapter.PgError
	if errors.As(e, &p) {
		switch p.Code {
		case "55P03", "40P01", "40001":
			return contract.ErrBusy
		case "P0002", "42501":
			return contract.ErrNotFound
		case "22023", "22P02", "23514", "23503":
			return contract.ErrInvalid
		case "55000", "23505":
			return contract.ErrConflict
		}
	}
	return contract.ErrUnavailable
}
