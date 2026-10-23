//go:build pgx

// Responsibility: exercise compensation through authenticated Fake facts.
// Dependencies: migrated PostgreSQL and the production payment fixture.
// Caller: CI pgx lane; invariant: terminal identities cannot resurrect money.
package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestRefundFinalizerSignedFacts(t *testing.T) {
	for _, first := range []string{"succeeded", "failed"} {
		t.Run(first, func(t *testing.T) {
			p := newPaymentFactFixture(t)
			p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
			send := func(event string, status payment.PaymentStatus) {
				t.Helper()
				var body map[string]any
				if err := json.Unmarshal(p.raw(event, status, 1), &body); err != nil {
					t.Fatal(err)
				}
				body["refund_id"] = "finalizer-refund"
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at))
				if err != nil || receipt.Status != "processed" {
					t.Fatalf("C19D2_SIGNED_TERMINAL_RED %s: %+v %v", event, receipt, err)
				}
			}
			if first == "succeeded" {
				send("success", payment.StatusPartiallyRefunded)
			}
			send("failure", payment.StatusFailed)
			before := p.witness(t)
			for _, fact := range []struct {
				event  string
				status payment.PaymentStatus
			}{
				{"failure", payment.StatusFailed}, {"cancel-alias", payment.StatusCanceled},
				{"stale-success", payment.StatusPartiallyRefunded},
			} {
				send(fact.event, fact.status)
			}
			after := p.witness(t)
			if after.refunded != 0 || after.checkout.cash != p.start.AmountMinor || after.checkout.state != "paid" {
				t.Fatalf("C19D2_NET_CAPACITY_RED: %+v", after)
			}
			if before.checkout != after.checkout || p.providerCommands.Load() != 0 {
				t.Fatal("terminal replay changed money or issued a provider command")
			}
			var reversals int
			var components int64
			err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1),
 refunded_net_minor+refunded_tax_minor+refunded_shipping_minor
 FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2`, p.f.tenant, p.start.OrderID).Scan(&reversals, &components)
			})
			want := 0
			if first == "succeeded" {
				want = 1
			}
			if err != nil || reversals != want || components != 0 {
				t.Fatalf("C19D2_EXACTLY_ONCE_RED: reversals=%d components=%d err=%v", reversals, components, err)
			}
			// A fresh ordinary identity can consume exactly the restored capacity.
			p.receive(t, "replacement-admitted", payment.StatusRefunded, p.start.AmountMinor)
			if w := p.witness(t); w.refunded != p.start.AmountMinor {
				t.Fatalf("capacity not restored: %+v", w)
			}
		})
	}
}
