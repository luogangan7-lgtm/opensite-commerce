//go:build pgx

// Responsibility: controlled active-tenant enumeration for the outbox Runner.
// Dependencies: generated db query, pgxadapter.QueuePool and UUID validation;
// called by cmd/worker composition. No raw table SELECT or tenant authorization
// comes from event JSON; the app_outbox_worker pool has only broker EXECUTE rights.
package outbox

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type PGTenantSource struct{ Pool *pgxadapter.QueuePool }

func (s PGTenantSource) ActiveTenants(ctx context.Context, after string, limit int32) ([]string, error) {
	if s.Pool == nil || limit < 1 || limit > 1000 || after != "" && !tenanttx.ValidTenantID(after) {
		return nil, ErrInvalidInput
	}
	var cursor pgtype.UUID
	if after != "" {
		if err := cursor.Scan(after); err != nil {
			return nil, ErrInvalidInput
		}
	}
	return db.New(s.Pool).ListOutboxActiveTenants(ctx, db.ListOutboxActiveTenantsParams{AfterTenant: cursor, TenantLimit: limit})
}
