//go:build pgx

// Responsibility: lend one runtime transaction after trusted provider-account
// lookup. Dependencies: pgxadapter pool and generated resolver query. Bootstrap
// exposes this only to MAC-authenticated Payments; URL/payload tenants are ignored.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"time"
)

func (r *QueryRunner) RunFakePaymentTx(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
	if r == nil || r.runner.pool == nil || fn == nil {
		return errors.New("payment database unavailable")
	}
	return pgx.BeginFunc(ctx, r.runner.pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); e != nil {
			return e
		}
		mapped, e := db.New(tx).ResolveFakePaymentAccount(ctx, account)
		if errors.Is(e, pgx.ErrNoRows) {
			return apperrors.New(401, "unauthenticated", "Payment account not recognized")
		}
		if e != nil {
			return e
		}
		tenant := mapped.TenantID.String()
		if _, e = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); e != nil {
			return e
		}
		return fn(ctx, tx, tenant, mapped.AtTime.Time)
	})
}
