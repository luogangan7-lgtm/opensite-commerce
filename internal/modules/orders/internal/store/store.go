// Package store connects order commands to generated sqlc queries. It depends
// on pgx transactions and db, owns no pool or connection, and is called only by
// the private orders service. Requested tenants must match the already trusted
// transaction context; mismatches disclose no rows or identifiers.
package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
)

func UUID(id string) (pgtype.UUID, error) {
	var value pgtype.UUID
	err := value.Scan(id)
	return value, err
}
func Tenant(ctx context.Context, tx pgx.Tx, tenant string) (*db.Queries, error) {
	if tx == nil {
		return nil, contract.ErrInvalid
	}
	requested, err := UUID(tenant)
	if err != nil || !requested.Valid {
		return nil, contract.ErrInvalid
	}
	queries := db.New(tx).WithTx(tx)
	active, err := queries.GetOrdersTenantContext(ctx)
	if err != nil {
		return nil, MapError(err)
	}
	if !active.Valid || active.Bytes != requested.Bytes {
		return nil, contract.ErrNotFound
	}
	return queries, nil
}

// MapError keeps driver messages, schema details and other tenants out of the
// domain contract while retaining stable, actionable business error classes.
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var e *pgconn.PgError
	if !errors.As(err, &e) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return contract.ErrUnavailable
	}
	switch {
	case strings.Contains(e.Message, "OWNER_MISMATCH"):
		return contract.ErrReservationOwner
	case strings.Contains(e.Message, "KEY_REUSED"):
		return contract.ErrKeyReused
	case e.Code == "P0002":
		return contract.ErrNotFound
	case e.Message == "INVENTORY_INSUFFICIENT_STOCK":
		return contract.ErrStockUnavailable
	case e.Code == "22023" || e.Code == "22003" || e.Code == "23514" || e.Code == "23503" || e.Code == "22P02":
		return contract.ErrInvalid
	case e.Code == "40001" || e.Code == "40P01":
		return contract.ErrRetry
	case e.Code == "55000" || e.Code == "23505":
		return contract.ErrConflict
	case e.Code == "42501":
		return contract.ErrNotFound
	}
	return contract.ErrUnavailable
}
