// Responsibilities: enforce authorized tenant context before exposing sqlc in a
// transaction. Dependencies: platform stamp and sqlc; bootstrap/module runners
// call this adapter; a URL tenant alone must never authorize database access.
package pgxadapter

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func authorizedTenant(ctx context.Context, tenant string) error {
	stamped, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return err
	}
	if stamped != tenant {
		return apperrors.New(404, "not_found", "Resource not found")
	}
	return nil
}

// Queries is the public Config.Run-compatible alias of the central A4 runner.
func (r *QueryRunner) Queries(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	return r.RunAuthorized(ctx, tenant, fn)
}
