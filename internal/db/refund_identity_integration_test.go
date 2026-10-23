//go:build pgx

// Responsibility: bind refund delivery variants to one persisted financial fact.
// Dependencies: production mapped Payments and migrated PG checkout fixture.
// Caller: CI pgx lane. Invariants: independent delivery receipts, one journal,
// immutable refund amount, and durable unmatched dashboard facts without money.
package db_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestPaymentsRefundDeliveryIdentity(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	for _, event := range []string{"refund-created", "refund-updated", "refund-created"} {
		var body map[string]any
		_ = json.Unmarshal(p.raw(event, payment.StatusPartiallyRefunded, 1), &body)
		body["refund_id"] = "provider-refund-one"
		raw, _ := json.Marshal(body)
		receipt, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at))
		if err != nil || receipt.Status != "processed" {
			t.Fatalf("refund delivery did not share one fact: %s %+v %v", event, receipt, err)
		}
	}
	w := p.witness(t)
	if w.inbox != 3 || w.events != 2 || w.applied != 2 || w.refunded != 1 || w.checkout.journals != 2 || w.checkout.cash != p.start.AmountMinor-1 {
		t.Fatalf("duplicate provider fact reposted money: %+v", w)
	}
	var body map[string]any
	_ = json.Unmarshal(p.raw("changed-refund", payment.StatusPartiallyRefunded, 2), &body)
	body["refund_id"] = "provider-refund-one"
	raw, _ := json.Marshal(body)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("refund identity accepted a changed amount")
	}
	if after := p.f.witness(t, p.start.OrderID); after != w.checkout {
		t.Fatal("conflicting refund changed money")
	}
}

func TestStripeDashboardRefundDurableUnmatched(t *testing.T) {
	p := newPaymentFactFixture(t)
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment) VALUES($1,'stripe',$2,'test')", p.f.tenant, account)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "whsec_isolated_dashboard_test_signing_key"
	t.Setenv("OPENSITE_ENVIRONMENT", "test")
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	t.Setenv("STRIPE_TEST_WEBHOOK_SECRET", key)
	t.Setenv("STRIPE_TEST_ACCOUNT_ID", account)
	s, err := payments.NewStripe(payments.Config{Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
		return p.f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var tenant string
			var at time.Time
			if err := tx.QueryRow(ctx, "SELECT tenant_id::text,at_time FROM app.resolve_stripe_payment_account($1)", account).Scan(&tenant, &at); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true),set_config('app.payment_provider','stripe',true)", tenant); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx, tenant, at)
		})
	}, Apply: func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error) {
		t.Fatal("unmatched refund reached money application")
		return false, nil
	}, Admit: func(context.Context, payment.CreatePaymentIntent, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"evt_dashboard","type":"refund.updated","livemode":false,"data":{"object":{"id":"re_dashboard","payment_intent":"pi_dashboard","status":"succeeded","amount":300,"currency":"usd"}}}`)
	stamp := fmt.Sprint(p.at.Unix())
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	for i := 0; i < 2; i++ {
		receipt, err := s.Receive(p.f.ctx, raw, "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		if err != nil || receipt.Status != "unmatched" {
			t.Fatalf("dashboard fact silently lost: %+v %v", receipt, err)
		}
	}
	var facts, journals int
	err = p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider='stripe' AND provider_event_id='evt_dashboard' AND status='failed'),(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1)", p.f.tenant).Scan(&facts, &journals)
	})
	if err != nil || facts != 1 || journals != 0 {
		t.Fatalf("unmatched retention/journal: facts=%d journals=%d err=%v", facts, journals, err)
	}
}

func TestPaymentsLegacyRefundRequiresReconciliation(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	var body map[string]any
	_ = json.Unmarshal(p.raw("legacy-created", payment.StatusPartiallyRefunded, 1), &body)
	body["refund_id"] = "legacy-provider-refund"
	created, _ := json.Marshal(body)
	// Seed the previous parser's persisted representation: raw-body hash exists,
	// but the normalized fact discarded the provider refund ID and used event ID.
	identity, _ := json.Marshal([4]string{"fake", "test", p.account, "legacy-created"})
	digest, payload := sha256.Sum256(identity), sha256.Sum256(created)
	key := hex.EncodeToString(digest[:])
	err := p.run(p.f.ctx, p.account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
		q := db.New(tx)
		inbox, err := q.AdmitPaymentInbox(ctx, db.AdmitPaymentInboxParams{Account: p.account, EventID: "legacy-created", PayloadHash: payload[:]})
		if err != nil {
			return err
		}
		if err = q.InsertPaymentEvent(ctx, db.InsertPaymentEventParams{TenantID: mustPGUUID(t, tenant), ID: inbox, ProviderAccountID: p.account, ProviderRef: p.start.Payment.ProviderRef, EventKey: key, ClaimedOrderID: mustPGUUID(t, p.start.OrderID), OrderVersion: p.start.OrderVersion, Status: string(payment.StatusPartiallyRefunded), AmountMinor: 1, Currency: p.start.Currency}); err != nil {
			return err
		}
		done, err := p.f.service.(checkoutcontract.PaymentEventApplier).ApplyPaymentEvent(ctx, tx, payment.VerifiedEvent{EventID: key, TenantID: tenant, OrderID: p.start.OrderID, OrderVersion: p.start.OrderVersion, ProviderRef: p.start.Payment.ProviderRef, Status: payment.StatusPartiallyRefunded, Amount: money.Money{Amount: 1, Currency: p.start.Currency}})
		if err != nil || !done {
			return fmt.Errorf("seed legacy refund: applied=%v error=%v", done, err)
		}
		if err = q.SetPaymentEventOutcome(ctx, db.SetPaymentEventOutcomeParams{TenantID: mustPGUUID(t, tenant), ID: inbox, Applied: true}); err != nil {
			return err
		}
		return q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	before := p.f.witness(t, p.start.OrderID)
	body["event_id"] = "new-updated"
	raw, _ := json.Marshal(body)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("unbound legacy refund stream accepted a new financial identity")
	}
	if receipt, err := p.module.Receive(p.f.ctx, created, p.signature(created, p.at)); err != nil || receipt.Status != "processed" {
		t.Fatalf("legacy exact-body replay lost old financial identity: %+v %v", receipt, err)
	}
	if after := p.f.witness(t, p.start.OrderID); after != before {
		t.Fatal("legacy refund replay/update posted another journal")
	}
}

func TestPaymentsRefundAliasesConvergeAfterCapture(t *testing.T) {
	p := newPaymentFactFixture(t)
	for _, event := range []string{"before-created", "before-updated"} {
		var body map[string]any
		_ = json.Unmarshal(p.raw(event, payment.StatusPartiallyRefunded, 1), &body)
		body["refund_id"] = "before-provider-refund"
		raw, _ := json.Marshal(body)
		if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
			t.Fatal(err)
		}
	}
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	var pending int
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND status<>'processed'", p.f.tenant).Scan(&pending)
	})
	if err != nil || pending != 0 {
		t.Fatalf("refund aliases stranded after capture: %d %v", pending, err)
	}
	if w := p.witness(t); w.refunded != 1 || w.checkout.journals != 2 {
		t.Fatalf("out-of-order refund money: %+v", w)
	}
}
