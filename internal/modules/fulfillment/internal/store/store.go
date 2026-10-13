// Package store adapts fulfillment to generated SQL inside authorized txs.
// Dependencies: db and pgx; callers: private service. Invariants: requested tenant
// equals the trusted transaction GUC; driver/schema details never escape.
package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
)

func UUID(s string) pgtype.UUID { var id pgtype.UUID; _ = id.Scan(s); return id }
func Tenant(ctx context.Context, tx pgx.Tx, tenant string) (*db.Queries, error) {
	if tx == nil || !contract.ValidID(tenant) {
		return nil, contract.ErrInvalid
	}
	q := db.New(tx)
	active, err := q.GetFulfillmentTenantContext(ctx)
	if err != nil {
		return nil, MapError(err)
	}
	if active != UUID(tenant) {
		return nil, contract.ErrNotFound
	}
	return q, nil
}
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return contract.ErrUnavailable
	}
	switch {
	case strings.Contains(pg.Message, "KEY_REUSED"):
		return contract.ErrKeyReused
	case pg.Code == "23505" && pg.SchemaName == "app" && pg.TableName == "shipments" && pg.ConstraintName == "shipments_tenant_id_creation_key_key":
		return contract.ErrKeyReused
	case strings.Contains(pg.Message, "VERSION") || strings.Contains(pg.Message, "PRECONDITION"):
		return contract.ErrPrecondition
	case pg.Code == "P0002" || pg.Code == "42501":
		return contract.ErrNotFound
	case pg.Code == "40001" || pg.Code == "40P01":
		return contract.ErrRetry
	case pg.Code == "55000" || pg.Code == "23505" || pg.Code == "23514":
		return contract.ErrConflict
	case pg.Code == "22023" || pg.Code == "22003" || pg.Code == "23503" || pg.Code == "22P02":
		return contract.ErrInvalid
	default:
		return contract.ErrUnavailable
	}
}
