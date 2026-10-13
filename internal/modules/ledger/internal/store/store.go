// Package store is ledger's generated-query adapter over caller-owned pgx.Tx.
// Dependencies: SQLC db, pgx and ledger contract; caller: private ledger service.
// Tables: ledger_accounts/ledger_transactions/ledger_entries via owning queries.
// Invariants: no transaction lifecycle or tenant GUC changes; driver diagnostics
// are mapped into stable errors and cannot escape the public module boundary.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"time"
)

func uuid(id string) pgtype.UUID { var value pgtype.UUID; _ = value.Scan(id); return value }

func Post(ctx context.Context, tx pgx.Tx, input contract.Transaction) (string, error) {
	params := db.PostLedgerParams{TenantID: uuid(input.TenantID), Currency: input.Currency, SourceType: input.SourceType, SourceID: input.SourceID, Event: input.Event,
		Accounts: make([]pgtype.UUID, len(input.Entries)), Sides: make([]string, len(input.Entries)), Amounts: make([]int64, len(input.Entries))}
	for i, e := range input.Entries {
		params.Accounts[i] = uuid(e.AccountID)
		params.Sides[i] = string(e.Side)
		params.Amounts[i] = e.Amount.Amount
	}
	id, err := db.New(tx).PostLedger(ctx, params)
	if err != nil {
		return "", mapError(err)
	}
	if !id.Valid {
		return "", contract.ErrUnavailable
	}
	return id.String(), nil
}
func Balance(ctx context.Context, tx pgx.Tx, tenantID, accountID string, cutoff time.Time) (db.LedgerAccountBalanceRow, error) {
	row, err := db.New(tx).LedgerAccountBalance(ctx, db.LedgerAccountBalanceParams{TenantID: uuid(tenantID), AccountID: uuid(accountID), Cutoff: pgtype.Timestamptz{Time: cutoff, Valid: true}})
	if err != nil {
		return row, mapError(err)
	}
	return row, nil
}
func mapError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var pg *pgxadapter.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			if pg.ConstraintName == "ledger_idempotency_content" {
				return contract.ErrConflict
			}
		case "22003":
			return contract.ErrOverflow
		case "22023", "23514", "22001", "22P02":
			return contract.ErrInvalid
		case "42501":
			return contract.ErrForbidden
		case "40001", "40P01":
			return contract.ErrRetry
		case "23503":
			return contract.ErrNotFound
		}
	}
	return contract.ErrUnavailable
}
