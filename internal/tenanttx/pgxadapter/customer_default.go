//go:build !pgx

// Responsibility: keep explicit customer DB configuration fail-closed without pgx.
// Dependencies: sqlc port and tenanttx; callers: bootstrap/customer composition.
// Invariants: no fabricated capability, ignored configuration or DSN disclosure.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type CustomerRunner struct{}

func OpenCustomerRunner(context.Context, string) (*CustomerRunner, error) {
	return nil, errors.New("customer database configuration requires the pgx build tag")
}
func (*CustomerRunner) Close() {}
func (*CustomerRunner) InTenant(ctx context.Context, tenant string, _ func(*db.Queries) error) error {
	if !tenanttx.ValidTenantID(tenant) {
		return tenanttx.ErrInvalidTenant
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("customer database is unavailable")
}
