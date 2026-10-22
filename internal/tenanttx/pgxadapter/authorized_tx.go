//go:build pgx

// Responsibilities: check the HTTP authorization stamp before lending a tenant
// transaction. Dependencies: the central tenant checker and Runner; bootstrap
// composition calls this seam. A tenant supplied in a URL cannot authorize it.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

// RunAuthorizedTx exposes a transaction only after tenant authorization.
func (r Runner) RunAuthorizedTx(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if err := authorizedTenant(ctx, tenant); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("transaction callback is nil")
	}
	return r.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := httpapi.RevalidateTransaction(ctx, db.New(tx)); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}
