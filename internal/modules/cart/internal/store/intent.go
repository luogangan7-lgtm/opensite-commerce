// Responsibility: read frozen shopping intent inside an existing authorized tx.
// Dependencies: generated SQL and cart contract; caller: cart.ReadIntent.
// Invariants: matching tenant/runtime stamp, actor RLS, row lock before lines,
// exact restoration of caller actor GUCs; no transaction/pool or projected prices.
package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
)

func canonicalID(value string) bool {
	u := uuid(value)
	return u.Valid && u.Bytes != [16]byte{} && id(u) == value
}

// ReadIntent borrows only the authorized transaction. It never changes its role
// and restores all three actor GUCs even on a failed read. Any failure, including
// context restoration, requires the caller to roll back the borrowed transaction.
func ReadIntent(ctx context.Context, tx pgx.Tx, actor contract.AuthorizedActor, cartID string) (out contract.Intent, err error) {
	guest := actor.CustomerID == "" && len(actor.GuestTokenHash) == 32
	customer := canonicalID(actor.CustomerID) && len(actor.GuestTokenHash) == 0
	if tx == nil || !canonicalID(actor.TenantID) || !canonicalID(cartID) || (!guest && !customer) {
		return out, unavailable()
	}
	q := db.New(tx)
	prior, err := q.GetCartContext(ctx)
	if err != nil || prior.TenantContext != actor.TenantID || prior.RuntimeRole != "app_runtime" {
		return out, unavailable()
	}
	defer func() {
		if e := q.SetCartContext(ctx, db.SetCartContextParams{TenantContext: prior.TenantContext, CustomerContext: prior.CustomerContext, TokenContext: prior.TokenContext}); e != nil {
			out, err = contract.Intent{}, unavailable()
		}
	}()
	if err = q.SetCartContext(ctx, db.SetCartContextParams{TenantContext: actor.TenantID, CustomerContext: actor.CustomerID, TokenContext: hex.EncodeToString(actor.GuestTokenHash)}); err != nil {
		return out, unavailable()
	}
	row, err := q.GetCartForUpdate(ctx, db.GetCartForUpdateParams{TenantID: uuid(actor.TenantID), ID: uuid(cartID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, missing()
	}
	if err != nil {
		return out, unavailable()
	}
	if id(row.TenantID) != actor.TenantID || id(row.ID) != cartID || row.Version <= 0 ||
		(customer && (id(row.CustomerID) != actor.CustomerID || len(row.GuestTokenHash) != 0)) ||
		(guest && (row.CustomerID.Valid || !bytes.Equal(row.GuestTokenHash, actor.GuestTokenHash))) {
		return out, missing()
	}
	lines, err := q.ListCartLines(ctx, db.ListCartLinesParams{TenantID: row.TenantID, CartID: row.ID})
	if err != nil || len(lines) > contract.MaxLines {
		return out, unavailable()
	}
	out = contract.Intent{ID: cartID, TenantID: actor.TenantID, Currency: row.Currency, Version: row.Version}
	for _, line := range lines {
		if id(line.TenantID) != actor.TenantID || id(line.CartID) != cartID || !canonicalID(id(line.SkuID)) || line.Quantity < 1 || line.Quantity > contract.MaxQuantity {
			return contract.Intent{}, unavailable()
		}
		out.Lines = append(out.Lines, contract.IntentLine{SKUID: id(line.SkuID), Quantity: line.Quantity})
	}
	return out, nil
}
