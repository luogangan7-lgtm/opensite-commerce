//go:build pgx

// Responsibility: exercise retained refund cases and recovery against production SQL.
// Dependencies: migrated PG and mapped payment fixtures; caller: CI pgx lanes.
// Invariants: replay commits one compensation; case history is immutable and tenant-bound.
package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestRefundCaseRecovery(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	var body map[string]any
	_ = json.Unmarshal(p.raw("recovery-success", payment.StatusPartiallyRefunded, 1), &body)
	body["refund_id"] = "recovery-refund"
	raw, _ := json.Marshal(body)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("retained authenticated terminal fact"))
	fact := map[string]any{"account_id": p.account, "refund_id": "recovery-refund", "provider_ref": p.start.Payment.ProviderRef,
		"event_id": "recovery-terminal", "payload_hash": hex.EncodeToString(hash[:]), "reason": "unmatched_refund"}
	var caseID string
	retain := func() error {
		return p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
			b, _ := json.Marshal(fact)
			return tx.QueryRow(ctx, "SELECT app.payments_retain_refund_case($1::jsonb)::text", b).Scan(&caseID)
		})
	}
	if err := retain(); err != nil {
		t.Fatalf("C19D3_RETAIN_RED: %v", err)
	}
	first := caseID
	if err := retain(); err != nil || caseID != first {
		t.Fatalf("case dedup: %s %s %v", first, caseID, err)
	}
	before := p.witness(t)
	for i := 0; i < 2; i++ {
		err := p.runTerminal(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
			b, _ := json.Marshal(map[string]any{"tenant_id": tenant, "provider": "fake", "account_id": p.account,
				"refund_id": "recovery-refund", "provider_ref": p.start.Payment.ProviderRef,
				"event_id": "recovery-terminal", "payload_hash": hex.EncodeToString(hash[:]), "status": "failed",
				"amount_minor": 1, "currency": p.start.Currency, "order_version": 0})
			_, err := tx.Exec(ctx, "SELECT app.payments_replay_refund_case($1::uuid,$2::jsonb)", caseID, b)
			return err
		})
		if err != nil {
			t.Fatalf("C19D3_REPLAY_RED iteration=%d: %v", i, err)
		}
	}
	after := p.witness(t)
	if before.refunded != 1 || after.refunded != 0 || after.checkout.cash != before.checkout.cash+1 {
		t.Fatalf("C19D3_DOUBLE_COMPENSATION_RED before=%+v after=%+v", before, after)
	}
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		var cases, links, resolutions, reversals int
		if err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.payment_refund_cases WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_refund_case_deliveries WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_refund_case_resolutions WHERE tenant_id=$1 AND outcome='resolved_compensated'),
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1)`, p.f.tenant).Scan(&cases, &links, &resolutions, &reversals); err != nil {
			return err
		}
		if cases != 1 || links != 1 || resolutions != 1 || reversals != 1 {
			t.Fatalf("C19D3_EXACTLY_ONCE_RED cases=%d links=%d resolutions=%d reversals=%d", cases, links, resolutions, reversals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fact["payload_hash"] = hex.EncodeToString(make([]byte, 32))
	if err := retain(); err == nil {
		t.Fatal("changed retained payload accepted")
	}
}
