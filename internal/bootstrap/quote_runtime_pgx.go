//go:build pgx

// Responsibility: privately compose reviewed runtime transaction capabilities.
// Dependencies: pgxadapter Open/Runner only; caller: production bootstrap startup.
// Invariants: effective app_runtime; HTTP quote callers need admitted actors,
// and payment callbacks enter only through the service's signed-event protocol.
package bootstrap

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func openQuoteRuntime(ctx context.Context, dsn string) (runtimeQuoteRunner, func(), error) {
	runner, close, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		return runtimeQuoteRunner{}, nil, errors.New("quote checkout runtime capability unavailable")
	}
	withRole := func(run checkoutcontract.Run) checkoutcontract.Run {
		return func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
			return run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
					return err
				}
				if err := httpapi.RevalidateTransaction(ctx, db.New(tx)); err != nil {
					return err
				}
				return fn(ctx, tx)
			})
		}
	}
	return runtimeQuoteRunner{run: withRole(runner.InTenant), snapshot: withRole(runner.InTenantSnapshot)}, close, nil
}
