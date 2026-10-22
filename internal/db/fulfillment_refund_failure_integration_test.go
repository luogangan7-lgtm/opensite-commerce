//go:build pgx

// Responsibilities: persist known refund rejection without admitting stale completion.
// Dependencies: public Returns fixture and controlled SQL commands through pgxadapter.
// Callers: PG18 CI gate. Invariants: only trusted service can fail a pending intent;
// uncertain errors cannot release it; corrected retries use a new immutable PSP key.
package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

func TestRefundFailedIntentGuard(t *testing.T) {
	f := newFulfillmentFixture(t)
	var ret returnscontract.Return
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ret, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: "failure-request", Kind: returnscontract.UnshippedCancel, Reason: "failure guard", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 2}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var accounts returnscontract.RefundAccounts
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='sales'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='tax'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='shipping'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='cash')`, f.tenant).Scan(&accounts.Income, &accounts.Tax, &accounts.Shipping, &accounts.Cash)
	})
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(key string, version int64) error {
		return f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var raw []byte
			err := tx.QueryRow(ctx, `SELECT app.returns_prepare_refund($1,$2,$3,'capture-proof',1,$4,$5,$6,$7)`, ret.ID, version, key, accounts.Income, accounts.Tax, accounts.Shipping, accounts.Cash).Scan(&raw)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, &ret)
		})
	}
	if err = prepare("failed-key", ret.OrderVersion); err != nil {
		t.Fatal(err)
	}
	fail := func(runner fulfillmentRunner, key, code string) error {
		return runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT app.returns_fail_refund($1,$2,$3)", ret.ID, key, code)
			return err
		})
	}
	if err = fail(f.runtime, "failed-key", "amount_mismatch"); err == nil {
		t.Fatal("runtime can release refund intent")
	}
	if err = fail(f.service, "wrong-key", "amount_mismatch"); err == nil {
		t.Fatal("wrong intent can release quota")
	}
	if err = fail(f.service, "failed-key", "retryable"); err == nil {
		t.Fatal("uncertain outcome can release quota")
	}
	if err = fail(f.service, "failed-key", "amount_mismatch"); err != nil {
		t.Fatalf("known rejection cannot enter refund_failed: %v", err)
	}
	var status string
	var version, confirmed, pending int64
	snapshot := func() {
		t.Helper()
		err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT r.status,o.version,o.refunded_minor,
			 (SELECT coalesce(sum(refund_minor),0)::bigint FROM app.returns WHERE tenant_id=$1 AND order_id=$2 AND status='refund_pending')
			 FROM app.returns r JOIN app.orders o ON o.tenant_id=r.tenant_id AND o.id=r.order_id WHERE r.tenant_id=$1 AND r.id=$3`, f.tenant, f.order.ID, ret.ID).Scan(&status, &version, &confirmed, &pending)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot()
	if status != "refund_failed" || confirmed != 0 || pending != 0 {
		t.Fatalf("rejection was not durable/released: %s confirmed=%d pending=%d", status, confirmed, pending)
	}
	failedVersion := version
	if err = fail(f.service, "failed-key", "amount_mismatch"); err != nil {
		t.Fatal(err)
	}
	snapshot()
	if version != failedVersion {
		t.Fatal("failure replay changed order version")
	}
	err = f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT app.returns_lock_refund($1,'failed-key')", ret.ID)
		return err
	})
	if err == nil {
		t.Fatal("failed intent accepted as successful provider completion")
	}
	if err = prepare("corrected-key", version); err != nil {
		t.Fatalf("corrected intent cannot reserve released capacity: %v", err)
	}
	if err = fail(f.service, "failed-key", "amount_mismatch"); err == nil {
		t.Fatal("stale rejection released corrected intent")
	}
	snapshot()
	if status != "refund_pending" || confirmed != 0 || pending != ret.RefundMinor {
		t.Fatalf("corrected pending intent corrupted: %s %d %d", status, confirmed, pending)
	}
}
