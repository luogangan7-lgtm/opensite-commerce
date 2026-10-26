//go:build pgx

// Responsibilities: persist known refund rejection without admitting stale completion.
// Dependencies: public Returns fixture and controlled SQL commands through pgxadapter.
// Callers: PG18 CI gate. Invariants: only trusted service can fail a pending intent;
// uncertain errors retain it; corrected retries use a new immutable PSP key.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	ledgercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
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
	fail := func(runner fulfillmentRunner, key string, rejected bool) error {
		return runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT app.returns_fail_refund($1,$2,$3)", ret.ID, key, rejected)
			return err
		})
	}
	if err = fail(f.runtime, "failed-key", true); err == nil {
		t.Fatal("runtime can release refund intent")
	}
	if err = fail(f.service, "wrong-key", true); err == nil {
		t.Fatal("wrong intent can release quota")
	}

	var status string
	var version, confirmed, pending int64
	snapshot := func() {
		t.Helper()
		err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT r.status,o.version,o.refunded_minor,
			 (SELECT coalesce(sum(refund_minor),0)::bigint FROM app.returns WHERE tenant_id=$1 AND order_id=$2 AND status IN('refund_pending','refund_reconcile'))
			 FROM app.returns r JOIN app.orders o ON o.tenant_id=r.tenant_id AND o.id=r.order_id WHERE r.tenant_id=$1 AND r.id=$3`, f.tenant, f.order.ID, ret.ID).Scan(&status, &version, &confirmed, &pending)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = fail(f.service, "failed-key", false); err != nil {
		t.Fatalf("uncertain outcome cannot enter reconciliation: %v", err)
	}
	snapshot()
	if status != "refund_reconcile" || confirmed != 0 || pending != ret.RefundMinor {
		t.Fatalf("uncertain refund lost reserved quota: %s %d %d", status, confirmed, pending)
	}
	if err = prepare("unsafe-new-key", version); err == nil {
		t.Fatal("uncertain refund admits a new provider key")
	}
	if err = fail(f.service, "failed-key", true); err != nil {
		t.Fatalf("known rejection cannot enter refund_failed: %v", err)
	}
	snapshot()
	if status != "refund_failed" || confirmed != 0 || pending != 0 {
		t.Fatalf("rejection was not durable/released: %s confirmed=%d pending=%d", status, confirmed, pending)
	}
	failedVersion := version
	if err = fail(f.service, "failed-key", true); err != nil {
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
	if err = fail(f.service, "failed-key", true); err == nil {
		t.Fatal("stale rejection released corrected intent")
	}
	snapshot()
	if status != "refund_pending" || confirmed != 0 || pending != ret.RefundMinor {
		t.Fatalf("corrected pending intent corrupted: %s %d %d", status, confirmed, pending)
	}
}

func TestRefundRejectedThenCorrected(t *testing.T) {
	f := newFulfillmentFixture(t)
	var ret returnscontract.Return
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ret, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: "rejected-request", Kind: returnscontract.UnshippedCancel, Reason: "corrected refund", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 2}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.payments.FailNext(paymentfake.Refund, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "fixture no-effect rejection"})
	failed := returnscontract.RefundInput{ReturnID: ret.ID, ExpectedVersion: ret.OrderVersion, IdempotencyKey: "rejected-refund"}
	if _, err = f.returns.Refund(f.ctx, f.tenant, failed); err == nil {
		t.Fatal("provider rejection reported success")
	}
	var status string
	var version, confirmed, journals int64
	snapshot := func() {
		t.Helper()
		err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT r.status,o.version,o.refunded_minor,
			 (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='return' AND source_id=$3::uuid::text)
			 FROM app.returns r JOIN app.orders o ON o.tenant_id=r.tenant_id AND o.id=r.order_id WHERE r.tenant_id=$1 AND r.order_id=$2 AND r.id=$3::uuid`, f.tenant, f.order.ID, ret.ID).Scan(&status, &version, &confirmed, &journals)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot()
	if status != "refund_failed" || confirmed != 0 || journals != 0 || len(f.payments.Events()) != 2 {
		t.Fatalf("rejection has financial effects: status=%s confirmed=%d journals=%d events=%d", status, confirmed, journals, len(f.payments.Events()))
	}
	if _, err = f.returns.Refund(f.ctx, f.tenant, failed); !errors.Is(err, returnscontract.ErrConflict) {
		t.Fatalf("failed same-key intent revived: %v", err)
	}
	corrected := returnscontract.RefundInput{ReturnID: ret.ID, ExpectedVersion: version, IdempotencyKey: "corrected-refund"}
	for i := 0; i < 2; i++ {
		out, err := f.returns.Refund(f.ctx, f.tenant, corrected)
		if err != nil || out.Status != "refunded" || out.RefundMinor != ret.RefundMinor {
			t.Fatalf("corrected refund/replay %d: %+v %v", i, out, err)
		}
	}
	snapshot()
	if status != "refunded" || confirmed != ret.RefundMinor || journals != 1 || len(f.payments.Events()) != 3 || f.stock(t) != 18 {
		t.Fatalf("corrected refund did not conserve effects: status=%s confirmed=%d journals=%d events=%d stock=%d", status, confirmed, journals, len(f.payments.Events()), f.stock(t))
	}
}

// A valid UUID with no matching exact journal must fail the real DB receipt check.
type mismatchedReturnJournal struct { *failingRefundJournal }
func (mismatchedReturnJournal) PostReturnRefund(context.Context, pgx.Tx, string, string, string, ledgercontract.Transaction) (string, error) {
	return uuid.NewString(), nil
}
func TestReturnsPGTrustedLedgerFailureRecovery(t *testing.T) {
	for _, failure := range []string{"account binding", "exact journal"} {
		t.Run(failure, func(t *testing.T) {
			f := newFulfillmentFixture(t)
			var ret returnscontract.Return
			if err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				ret, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: "trusted-ledger-request", Kind: returnscontract.UnshippedCancel, Reason: "trusted error status", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 1}}})
				return err
			}); err != nil { t.Fatal(err) }
			input := returnscontract.RefundInput{ReturnID: ret.ID, ExpectedVersion: ret.OrderVersion, IdempotencyKey: "trusted-ledger-refund"}
			setAccountStatus := func(status string) {
				t.Helper()
				if err := f.maint.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, "UPDATE app.ledger_accounts SET status=$2 WHERE tenant_id=$1 AND code='sales'", f.tenant, status)
					return err
				}); err != nil { t.Fatal(err) }
			}
			if failure == "account binding" {
				setAccountStatus("inactive")
				if _, err := f.returns.Refund(f.ctx, f.tenant, input); !errors.Is(err, returnscontract.ErrUnavailable) { t.Fatalf("trusted account failure must be unavailable: %v", err) }
				if len(f.payments.Events()) != 2 { t.Fatal("invalid trusted account reached provider") }
				setAccountStatus("active")
			} else {
				f.journal.fail = true
				if _, err := f.returns.Refund(f.ctx, f.tenant, input); !errors.Is(err, returnscontract.ErrUnavailable) { t.Fatalf("post-PSP failure must be unavailable: %v", err) }
				f.returns = returns.New(returns.Config{Runner: f.service, Payments: f.payments, Ledger: mismatchedReturnJournal{f.journal}})
				if _, err := f.returns.Refund(f.ctx, f.tenant, input); !errors.Is(err, returnscontract.ErrUnavailable) { t.Fatalf("exact DB journal mismatch must be unavailable: %v", err) }
				if len(f.payments.Events()) != 3 { t.Fatal("journal failure repeated provider refund") }
				f.returns = returns.New(returns.Config{Runner: f.service, Payments: f.payments, Ledger: f.journal})
			}
			out, err := f.returns.Refund(f.ctx, f.tenant, input)
			if err != nil || out.Status != "refunded" || len(f.payments.Events()) != 3 { t.Fatalf("trusted posting same-key recovery: %+v %v", out, err) }
			if _, err := f.returns.Refund(f.ctx, f.tenant, input); err != nil || len(f.payments.Events()) != 3 { t.Fatalf("completed retry repeated refund: %v", err) }
			var journals int
			if err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='return' AND source_id=$2", f.tenant, ret.ID).Scan(&journals)
			}); err != nil || journals != 1 { t.Fatalf("same-key recovery duplicated journal: count=%d err=%v", journals, err) }
		})
	}
}
