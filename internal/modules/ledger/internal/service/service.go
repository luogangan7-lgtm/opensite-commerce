// Package service validates ledger commands and computes exact normal balances.
// Dependencies: ledger contract/store, pgx and money; caller: ledger public port.
// Invariants: caller owns transaction lifecycle and tenant authority; no SQL/pool.
package service

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"time"
)

func Post(ctx context.Context, tx pgx.Tx, input contract.Transaction) (string, error) {
	if err := input.Validate(); err != nil {
		return "", err
	}
	if tx == nil {
		return "", contract.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return store.Post(ctx, tx, input)
}

func Balance(ctx context.Context, tx pgx.Tx, tenantID, accountID string, cutoff time.Time) (contract.Balance, error) {
	if tx == nil || !contract.ValidID(tenantID) || !contract.ValidID(accountID) || cutoff.IsZero() {
		return contract.Balance{}, contract.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return contract.Balance{}, err
	}
	row, err := store.Balance(ctx, tx, tenantID, accountID, cutoff)
	if err != nil {
		return contract.Balance{}, err
	}
	result := contract.Balance{Currency: row.Currency, Debits: money.Money{Amount: row.Debits, Currency: row.Currency}, Credits: money.Money{Amount: row.Credits, Currency: row.Currency}}
	if row.Debits < 0 || row.Credits < 0 {
		return contract.Balance{}, contract.ErrUnavailable
	}
	switch row.AccountType {
	case "asset", "expense":
		result.NormalNet, err = result.Debits.Sub(result.Credits)
	case "liability", "equity", "revenue":
		result.NormalNet, err = result.Credits.Sub(result.Debits)
	default:
		return contract.Balance{}, contract.ErrUnavailable
	}
	if err != nil {
		return contract.Balance{}, contract.ErrOverflow
	}
	return result, nil
}
