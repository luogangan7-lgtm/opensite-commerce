//go:build pgx

// Responsibility: exercise compensation through authenticated Fake facts.
// Dependencies: migrated PostgreSQL and the production payment fixture.
// Caller: CI pgx lane; invariant: terminal identities cannot resurrect money.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"testing"
	"time"

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
				before := p.witness(t)
				abort := errors.New("C19D2 after all projections before commit")
				m, err := payments.New(payments.Config{Key: p.key, Run: p.run, Apply: p.f.service.(checkoutcontract.PaymentEventApplier).ApplyPaymentEvent,
					TerminalRun: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
						return p.runTerminal(ctx, account, func(ctx context.Context, tx pgx.Tx, tenant string, at time.Time) error {
							if err := fn(ctx, tx, tenant, at); err != nil {
								return err
							}
							return abort
						})
					}})
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				_ = json.Unmarshal(p.raw("abort-before-commit", payment.StatusFailed, 1), &body)
				body["refund_id"] = "finalizer-refund"
				raw, _ := json.Marshal(body)
				if _, err = m.Receive(p.f.ctx, raw, p.signature(raw, p.at)); !errors.Is(err, abort) {
					t.Fatalf("C19D2_ROLLBACK_RED: %v", err)
				}
				if p.witness(t) != before {
					t.Fatal("C19D2_ROLLBACK_RED partial money or Inbox commit")
				}
				p.rejectCompensationMutation(t, "app_orders_executor", "UPDATE app.orders SET refunded_minor=refunded_minor-1,state='paid',version=version+1 WHERE tenant_id=$1")
				p.rejectCompensationMutation(t, "app_payments_service", "UPDATE app.checkout_sessions SET refunded_net_minor=refunded_net_minor-1 WHERE tenant_id=$1")
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

func (p *paymentFactFixture) runTerminal(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
	return p.run(ctx, account, func(ctx context.Context, tx pgx.Tx, tenant string, at time.Time) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_payments_ingress"); err != nil {
			return err
		}
		return fn(ctx, tx, tenant, at)
	})
}

func (p *paymentFactFixture) terminalFact(t *testing.T, event, refund string, amount int64) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(p.raw(event, payment.StatusFailed, amount), &body); err != nil {
		t.Fatal(err)
	}
	body["refund_id"] = refund
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at))
	if err != nil || receipt.Status != "processed" {
		t.Fatalf("C19D2_MIXED_TERMINAL_RED: %+v %v", receipt, err)
	}
}

func (p *paymentFactFixture) rejectCompensationMutation(t *testing.T, role, statement string) {
	t.Helper()
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, statement, p.f.tenant)
		return err
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("C19D2_INDEPENDENT_WITNESS_RED %s: %v", role, err)
	}
}
