//go:build pgx

// Responsibility: regress FIX-341 original PSP binding and compensation entry fences.
// Dependencies: production mapped ingress and closed Returns/Payments SQL ports.
// Caller: CI pgx lane; unproven facts must retain money and reconciliation access.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestRefundFinalizerOriginalRMABinding(t *testing.T) {
	p := newPaymentFactFixture(t)
	stripeAccount := "acct_original_stripe"
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment) VALUES($1,'stripe',$2,'test')", p.f.tenant, stripeAccount)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Seed an authenticated Stripe capture through mapped admission and the real
	// Checkout applier. The attack below is a genuinely signed Fake delivery.
	if err := p.run(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.payment_provider','stripe',true)"); err != nil {
			return err
		}
		q := db.New(tx)
		inbox, err := q.AdmitPaymentInbox(ctx, db.AdmitPaymentInboxParams{Account: stripeAccount, EventID: "stripe-capture", PayloadHash: make([]byte, 32)})
		if err != nil {
			return err
		}
		key := "3410000000000000000000000000000000000000000000000000000000000000"
		if err = q.InsertPaymentEvent(ctx, db.InsertPaymentEventParams{TenantID: mustPGUUID(t, tenant), ID: inbox, ProviderAccountID: stripeAccount, ProviderRef: p.start.Payment.ProviderRef, EventKey: key, ClaimedOrderID: mustPGUUID(t, p.start.OrderID), OrderVersion: p.start.OrderVersion, Status: "captured", AmountMinor: p.start.AmountMinor, Currency: p.start.Currency}); err != nil {
			return err
		}
		if _, err = p.f.service.(checkoutcontract.PaymentEventApplier).ApplyPaymentEvent(ctx, tx, payment.VerifiedEvent{EventID: key, TenantID: tenant, OrderID: p.start.OrderID, OrderVersion: p.start.OrderVersion, ProviderRef: p.start.Payment.ProviderRef, Status: payment.StatusCaptured, Amount: money.Money{Amount: p.start.AmountMinor, Currency: p.start.Currency}}); err != nil {
			return err
		}
		if err = q.SetPaymentEventOutcome(ctx, db.SetPaymentEventOutcomeParams{TenantID: mustPGUUID(t, tenant), ID: inbox, Applied: true}); err != nil {
			return err
		}
		return q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: true})
	}); err != nil {
		t.Fatal(err)
	}
	var amount int64
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
			return err
		}
		// No success webhook is needed: original authority must be saved when
		// the synchronous RMA refund posts, before any terminal observation.
		_, err := tx.Exec(ctx, `DO $fixture$
DECLARE c app.checkout_sessions; o app.orders; r jsonb; rid uuid; journal uuid;
BEGIN
 SELECT * INTO STRICT c FROM app.checkout_sessions WHERE tenant_id=current_setting('app.tenant_id')::uuid;
 SELECT * INTO STRICT o FROM app.orders WHERE tenant_id=c.tenant_id AND id=c.order_id;
 r:=app.returns_create(o.id,o.version,'binding-rma','unshipped_cancel','binding regression',
  (SELECT jsonb_build_array(jsonb_build_object('order_line_id',id,'quantity',1)) FROM app.order_lines WHERE tenant_id=o.tenant_id AND order_id=o.id LIMIT 1));
 rid:=(r->>'id')::uuid;
 r:=app.returns_prepare_refund(rid,(r->>'order_version')::bigint,'binding-refund',c.provider_ref,c.payable_version,
  (SELECT id FROM app.ledger_accounts WHERE tenant_id=o.tenant_id AND code='revenue'),
  (SELECT id FROM app.ledger_accounts WHERE tenant_id=o.tenant_id AND code='tax_payable'),
  (SELECT id FROM app.ledger_accounts WHERE tenant_id=o.tenant_id AND code='shipping_revenue'),
  (SELECT id FROM app.ledger_accounts WHERE tenant_id=o.tenant_id AND code='cash'));
 journal:=app.payments_post_return_refund(rid,'binding-refund','re_original_stripe');
 PERFORM app.returns_complete_refund(rid,'binding-refund',c.provider_ref,journal);
END $fixture$`)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT refund_minor FROM app.returns WHERE tenant_id=$1", p.f.tenant).Scan(&amount)
	}); err != nil {
		t.Fatal(err)
	}
	p.rejectTerminalBinding(t, "fake-names-stripe", "re_original_stripe", amount)
	// The shared attempt guard must also refuse a caller relabelling the RMA.
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id,order_id,provider_ref,amount_minor,currency,return_id,attempt_key,original_journal_id)
 SELECT b.tenant_id,'fake',$2,'test',b.provider_refund_id,r.order_id,b.provider_ref,r.refund_minor,r.currency,b.return_id,b.attempt_key,b.journal_id
 FROM app.payment_return_refunds b JOIN app.returns r ON (r.tenant_id,r.id)=(b.tenant_id,b.return_id) WHERE b.tenant_id=$1`, p.f.tenant, p.account)
		return err
	})
	var pg interface{ SQLState() string }
	if !errors.As(err, &pg) || pg.SQLState() != "23514" {
		t.Errorf("FIX341_ATTEMPT_BINDING_RED: %v", err)
	}
	p.rejectCompensationMutation(t, "app_fulfillment_executor", `INSERT INTO app.returns
 SELECT (jsonb_populate_record(NULL::app.returns,to_jsonb(r)||jsonb_build_object(
 'id',gen_random_uuid(),'creation_key','forged-compensated','receive_key','forged-receive',
 'refund_key','forged-refund','status','refund_compensated'))).* FROM app.returns r WHERE tenant_id=$1`)
	// Rejection must leave the real provider able to finalize the original
	// refund without waiting for a success webhook or creating a new attempt.
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true),set_config('app.payment_provider','stripe',true)", p.f.tenant); err != nil {
			return err
		}
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT provider='stripe' AND provider_account_id=$2 AND environment='test'
 FROM app.payment_return_refunds WHERE tenant_id=$1`, p.f.tenant, stripeAccount).Scan(&bound); err != nil || !bound {
			return errors.New("FIX341 original Stripe account not persisted at refund time")
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_payments_ingress"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT app.payments_finalize_refund(jsonb_build_object(
 'tenant_id',$1::text,'provider','stripe','account_id',$2::text,'refund_id','re_original_stripe',
 'provider_ref',$3::text,'order_id',$4::text,'order_version',$5::bigint,'amount_minor',$6::bigint,
 'currency',$7::text,'event_id','stripe-original-terminal','payload_hash',repeat('3',64),
 'status','failed','timestamp',extract(epoch FROM clock_timestamp())::bigint))`,
			p.f.tenant, stripeAccount, p.start.Payment.ProviderRef, p.start.OrderID, p.start.OrderVersion, amount, p.start.Currency)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := p.f.witness(t, p.start.OrderID); w.cash != p.start.AmountMinor {
		t.Fatalf("FIX341 original provider did not restore exact capacity: %+v", w)
	}
}

func (p *paymentFactFixture) rejectTerminalBinding(t *testing.T, event, refund string, amount int64) {
	t.Helper()
	before := p.f.witness(t, p.start.OrderID)
	var body map[string]any
	if err := json.Unmarshal(p.raw(event, payment.StatusFailed, amount), &body); err != nil {
		t.Fatal(err)
	}
	body["refund_id"] = refund
	raw, _ := json.Marshal(body)
	for i := 0; i < 2; i++ {
		receipt, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at))
		if err == nil || receipt.Status != "rejected" {
			t.Errorf("FIX341_UNPROVEN_TERMINAL_RED %s: %+v %v", event, receipt, err)
		}
	}
	var failed, terminals, reversals int
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider_event_id=$2 AND status='failed' AND octet_length(payload_hash)=32),
 (SELECT count(*) FROM app.payment_refund_terminals WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1)`, p.f.tenant, event).Scan(&failed, &terminals, &reversals)
	})
	if err != nil || failed != 1 || terminals != 0 || reversals != 0 || p.f.witness(t, p.start.OrderID) != before {
		t.Errorf("FIX341_RECONCILIATION_RED %s failed=%d terminals=%d reversals=%d err=%v", event, failed, terminals, reversals, err)
	}
}
