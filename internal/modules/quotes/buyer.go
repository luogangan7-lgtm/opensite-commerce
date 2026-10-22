// Responsibility: resolve a quote's company for an already verified customer.
// Dependencies: own generated-query store and tenant runner; bootstrap admission
// calls this public port. No HTTP identity or company input is trusted here.
// Invariants: the same actor-filtered quote query checks live tenant/company/member;
// draft and foreign quotes are hidden before a trusted quote actor is constructed.
package quotes

import (
	"context"

	"github.com/jackc/pgx/v5"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/internal/store"
)

func ResolveBuyerForQuote(ctx context.Context, runner TenantRunner, tenant, customer, quoteID string) (identity qc.ActorIdentity, err error) {
	if !qc.ValidID(tenant) || !qc.ValidID(customer) || !qc.ValidID(quoteID) {
		return identity, qc.ErrInvalid
	}
	if runner == nil {
		return identity, qc.ErrUnavailable
	}
	err = runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		company, e := store.ResolveBuyerCompany(ctx, tx, tenant, customer, quoteID)
		if e == nil {
			identity = qc.ActorIdentity{TenantID: tenant, CustomerID: customer, CompanyID: company}
		}
		return e
	})
	return identity, err
}
