// Responsibility: expose quote-owned conversion commands on caller transactions.
// Dependencies: public quote contract/private store; caller: conversion application.
// Invariants: authentic actor/tenant, immutable negotiated values, no provider IO.
package quotes

import (
	"context"

	"github.com/jackc/pgx/v5"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/internal/store"
)

var _ c.ConversionRepository = (*Module)(nil)

func conversionGuard(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string, buyer bool) error {
	if !a.Valid() || (buyer && a.Kind() != "customer") {
		return c.ErrUnauthorized
	}
	if tx == nil {
		return c.ErrUnavailable
	}
	if !c.ValidID(quoteID) {
		return c.ErrInvalid
	}
	return ctx.Err()
}

func (m *Module) LockForConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Quote, error) {
	if err := conversionGuard(ctx, tx, a, quoteID, true); err != nil {
		return c.Quote{}, err
	}
	return store.LockForConversion(ctx, tx, a, quoteID)
}

func (m *Module) BindConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID, orderID string, version int64, key string, hash []byte) (c.Conversion, error) {
	if err := conversionGuard(ctx, tx, a, quoteID, true); err != nil {
		return c.Conversion{}, err
	}
	if !c.ValidID(orderID) || version < 1 || len(key) < 1 || len(key) > 128 || len(hash) != 32 {
		return c.Conversion{}, c.ErrInvalid
	}
	return store.BindConversion(ctx, tx, a, quoteID, orderID, version, key, hash)
}

func (m *Module) GetConversion(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, quoteID string) (c.Conversion, error) {
	if err := conversionGuard(ctx, tx, a, quoteID, false); err != nil {
		return c.Conversion{}, err
	}
	return store.GetConversion(ctx, tx, a, quoteID)
}

func (m *Module) ConversionSKU(ctx context.Context, tx pgx.Tx, a c.AuthorizedActor, productID, variantID string) (string, error) {
	if err := conversionGuard(ctx, tx, a, productID, true); err != nil {
		return "", err
	}
	if !c.ValidID(variantID) {
		return "", c.ErrInvalid
	}
	return store.ConversionSKU(ctx, tx, a, productID, variantID)
}
