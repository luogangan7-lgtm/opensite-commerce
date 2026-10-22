// Responsibility: company resolution through existing generated actor-filtered SQL.
// Dependencies: quote contract/sqlc/pgx; caller: public quotes.ResolveBuyerForQuote.
// Tables: read quotes through live membership policy; no pool or handwritten SQL.
package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
)

func ResolveBuyerCompany(ctx context.Context, tx pgx.Tx, tenant, customer, quoteID string) (string, error) {
	if tx == nil {
		return "", qc.ErrUnavailable
	}
	row, err := db.New(tx).GetQuote(ctx, db.GetQuoteParams{TenantID: id(tenant), QuoteID: id(quoteID), ActorKind: "customer", ActorID: id(customer)})
	if err != nil {
		return "", MapError(err)
	}
	return row.BuyerCompanyID.String(), nil
}
