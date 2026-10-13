// Responsibility: generated SQL conversion commands and stable receipt mapping.
// Dependencies: quote contract/sqlc/pgx; caller: public quotes conversion facade.
// Tables: quote_conversions and controlled quote/order reads; no pool or raw SQL.
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func conversionError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		if p.Code == "22000" {
			return c.ErrIdempotencyReused
		}
		if p.Code == "55000" {
			return c.ErrConflict
		}
	}
	return MapError(err)
}

func LockForConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Quote, error) {
	r, err := db.New(tx).LockQuoteConversion(ctx, db.LockQuoteConversionParams{
		TenantID: id(a.TenantID()), QuoteID: id(quoteID), CompanyID: id(a.CompanyID()), CustomerID: id(a.ID()),
	})
	return quote(r, err)
}

func BindConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID, orderID string, version int64, key string, hash []byte) (c.Conversion, error) {
	_, err := db.New(tx).BindQuoteConversion(ctx, db.BindQuoteConversionParams{
		TenantID: id(a.TenantID()), QuoteID: id(quoteID), CompanyID: id(a.CompanyID()), CustomerID: id(a.ID()),
		OrderID: id(orderID), QuoteVersion: version, IdempotencyKey: key, RequestHash: hash,
	})
	if err != nil {
		return c.Conversion{}, conversionError(err)
	}
	// A history replay can refer to an order that has since progressed.
	return GetConversion(ctx, tx, a, quoteID)
}

func GetConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Conversion, error) {
	r, err := db.New(tx).GetQuoteConversion(ctx, db.GetQuoteConversionParams{
		TenantID: id(a.TenantID()), QuoteID: id(quoteID), CompanyID: id(a.CompanyID()), ActorKind: a.Kind(), ActorID: id(a.ID()),
	})
	if err != nil {
		return c.Conversion{}, conversionError(err)
	}
	return conversion(db.AppQuoteConversion{QuoteID: r.QuoteID, OrderID: r.OrderID, BuyerCompanyID: r.BuyerCompanyID,
		CustomerID: r.CustomerID, QuoteVersion: r.QuoteVersion, OrderNumber: r.OrderNumber,
		IdempotencyKey: r.IdempotencyKey, Currency: r.Currency, TotalMinor: r.TotalMinor, CreatedAt: r.CreatedAt}, r.State), nil
}

func conversion(r db.AppQuoteConversion, state string) c.Conversion {
	return c.Conversion{QuoteID: r.QuoteID.String(), OrderID: r.OrderID.String(), CompanyID: r.BuyerCompanyID.String(),
		CustomerID: r.CustomerID.String(), QuoteVersion: r.QuoteVersion, OrderNumber: r.OrderNumber, IdempotencyKey: r.IdempotencyKey,
		Amount: money.Money{Amount: r.TotalMinor, Currency: r.Currency}, State: state, CreatedAt: r.CreatedAt.Time.UTC()}
}

func ConversionSKU(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, productID, variantID string) (string, error) {
	r, err := db.New(tx).GetQuoteConversionSKU(ctx, db.GetQuoteConversionSKUParams{
		TenantID: id(a.TenantID()), ProductID: id(productID), VariantID: id(variantID),
	})
	return r, conversionError(err)
}
