// Package store adapts return commands to generated SQL in an authorized tx.
// Dependencies: pgx/sqlc and public contract; callers: private return service.
// Invariants: tenant GUC matches requested tenant, intent reads are permanent,
// and raw driver/schema details are mapped to stable domain errors.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Repository struct{}

func UUID(s string) pgtype.UUID { var id pgtype.UUID; _ = id.Scan(s); return id }
func Tenant(ctx context.Context, tx pgx.Tx, tenant string) (*db.Queries, error) {
	if tx == nil || !contract.ValidID(tenant) {
		return nil, contract.ErrInvalid
	}
	q := db.New(tx)
	active, err := q.GetReturnsTenantContext(ctx)
	if err != nil {
		return nil, MapError(err)
	}
	if active != UUID(tenant) {
		return nil, contract.ErrNotFound
	}
	return q, nil
}
func decode(raw []byte) (contract.Return, error) {
	var out contract.Return
	if json.Unmarshal(raw, &out) != nil || !contract.ValidID(out.ID) || !contract.ValidID(out.OrderID) || out.OrderVersion <= 0 || out.Status == "" || (out.Kind != contract.ShipmentReturn && out.Kind != contract.UnshippedCancel) {
		return contract.Return{}, contract.ErrUnavailable
	}
	return out, nil
}
func (Repository) Request(ctx context.Context, tx pgx.Tx, tenant string, in contract.RequestInput) (contract.Return, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.Return{}, err
	}
	lines, _ := json.Marshal(in.Lines)
	raw, err := q.CreateReturn(ctx, db.CreateReturnParams{OrderID: UUID(in.OrderID), ExpectedVersion: in.ExpectedVersion, IdempotencyKey: in.IdempotencyKey, Kind: string(in.Kind), Reason: in.Reason, Lines: lines})
	if err != nil {
		return contract.Return{}, MapError(err)
	}
	return decode(raw)
}
func (Repository) Receive(ctx context.Context, tx pgx.Tx, tenant string, in contract.ReceiveInput) (contract.Return, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.Return{}, err
	}
	raw, err := q.ReceiveReturn(ctx, db.ReceiveReturnParams{ReturnID: UUID(in.ReturnID), ExpectedVersion: in.ExpectedVersion, IdempotencyKey: in.IdempotencyKey, Restock: in.Restock})
	if err != nil {
		return contract.Return{}, MapError(err)
	}
	return decode(raw)
}
func (Repository) OrderID(ctx context.Context, tx pgx.Tx, tenant, id string) (string, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return "", err
	}
	out, err := q.GetReturnOrderID(ctx, db.GetReturnOrderIDParams{TenantID: UUID(tenant), ReturnID: UUID(id)})
	if err != nil {
		return "", MapError(err)
	}
	return out, nil
}
func (Repository) OrderTotal(ctx context.Context, tx pgx.Tx, tenant, id string) (contract.CapturedPayment, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.CapturedPayment{}, err
	}
	out, err := q.GetReturnsOrderTotal(ctx, db.GetReturnsOrderTotalParams{TenantID: UUID(tenant), OrderID: UUID(id)})
	if err != nil {
		return contract.CapturedPayment{}, MapError(err)
	}
	return contract.CapturedPayment{TenantID: tenant, OrderID: id, Paid: money.Money{Amount: out.TotalMinor, Currency: out.Currency}}, nil
}
func (Repository) Prepared(ctx context.Context, tx pgx.Tx, tenant string, in contract.RefundInput) (contract.PreparedRefund, bool, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.PreparedRefund{}, false, err
	}
	raw, err := q.GetPreparedReturnRefund(ctx, db.GetPreparedReturnRefundParams{ReturnID: UUID(in.ReturnID), IdempotencyKey: in.IdempotencyKey})
	if err != nil {
		return contract.PreparedRefund{}, false, MapError(err)
	}
	var out contract.PreparedRefund
	if json.Unmarshal(raw, &out) != nil {
		return contract.PreparedRefund{}, false, contract.ErrUnavailable
	}
	return out, out.RefundKey != "", nil
}

// LockPrepared holds the controlled order/RMA locks through finalization.
func (Repository) LockPrepared(ctx context.Context, tx pgx.Tx, tenant string, in contract.RefundInput) (contract.PreparedRefund, bool, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.PreparedRefund{}, false, err
	}
	raw, err := q.LockReturnRefund(ctx, db.LockReturnRefundParams{ReturnID: UUID(in.ReturnID), IdempotencyKey: in.IdempotencyKey})
	if err != nil {
		return contract.PreparedRefund{}, false, MapError(err)
	}
	var out contract.PreparedRefund
	if json.Unmarshal(raw, &out) != nil {
		return contract.PreparedRefund{}, false, contract.ErrUnavailable
	}
	return out, out.RefundKey != "", nil
}
func (Repository) Prepare(ctx context.Context, tx pgx.Tx, tenant string, in contract.RefundInput, capture contract.CapturedPayment, accounts contract.RefundAccounts) (contract.PreparedRefund, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.PreparedRefund{}, err
	}
	raw, err := q.PrepareReturnRefund(ctx, db.PrepareReturnRefundParams{ReturnID: UUID(in.ReturnID), ExpectedVersion: in.ExpectedVersion, IdempotencyKey: in.IdempotencyKey, ProviderRef: capture.ProviderRef, PaymentVersion: capture.OrderVersion, IncomeAccountID: UUID(accounts.Income), TaxAccountID: UUID(accounts.Tax), ShippingAccountID: UUID(accounts.Shipping), CashAccountID: UUID(accounts.Cash)})
	if err != nil {
		return contract.PreparedRefund{}, MapError(err)
	}
	var out contract.PreparedRefund
	if json.Unmarshal(raw, &out) != nil {
		return contract.PreparedRefund{}, contract.ErrUnavailable
	}
	return out, nil
}
func (Repository) Complete(ctx context.Context, tx pgx.Tx, tenant string, in contract.PreparedRefund, journal string) (contract.Return, error) {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return contract.Return{}, err
	}
	raw, err := q.CompleteReturnRefund(ctx, db.CompleteReturnRefundParams{ReturnID: UUID(in.ID), IdempotencyKey: in.RefundKey, ProviderRef: in.ProviderRef, LedgerTransactionID: UUID(journal)})
	if err != nil {
		return contract.Return{}, MapError(err)
	}
	return decode(raw)
}

// Fail records the provider outcome under the same Order/RMA budget locks.
// The trusted adapter alone supplies proof that no side effect occurred.
func (Repository) Fail(ctx context.Context, tx pgx.Tx, tenant string, in contract.PreparedRefund, rejected bool) error {
	q, err := Tenant(ctx, tx, tenant)
	if err != nil {
		return err
	}
	_, err = q.FailReturnRefund(ctx, db.FailReturnRefundParams{ReturnID: UUID(in.ID), IdempotencyKey: in.RefundKey, Rejected: rejected})
	return MapError(err)
}

func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var pg *pgxadapter.PgError
	if !errors.As(err, &pg) {
		return contract.ErrUnavailable
	}
	switch {
	case strings.Contains(pg.Message, "KEY_REUSED"):
		return contract.ErrKeyReused
	case pg.Code == "23505" && pg.SchemaName == "app" && pg.TableName == "returns" && (pg.ConstraintName == "returns_tenant_id_creation_key_key" || pg.ConstraintName == "returns_tenant_id_receive_key_key" || pg.ConstraintName == "returns_tenant_id_refund_key_key"):
		return contract.ErrKeyReused
	case strings.Contains(pg.Message, "VERSION") || strings.Contains(pg.Message, "PRECONDITION"):
		return contract.ErrPrecondition
	case pg.Code == "P0002" || pg.Code == "42501":
		return contract.ErrNotFound
	case pg.Code == "40001" || pg.Code == "40P01":
		return contract.ErrRetry
	case pg.Code == "55000" || pg.Code == "23505" || pg.Code == "23514":
		return contract.ErrConflict
	case pg.Code == "22023" || pg.Code == "22003" || pg.Code == "23503" || pg.Code == "22P02":
		return contract.ErrInvalid
	default:
		return contract.ErrUnavailable
	}
}
