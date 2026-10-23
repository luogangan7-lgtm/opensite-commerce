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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
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

// Only intent creation is synthetic; webhook admission, Checkout and journals
// use their production implementations against the disposable database.
type stripeFactIntent struct{ payment.Port }

func (stripeFactIntent) CreateIntent(context.Context, payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ProviderRef: "pi_refundtransition", Status: payment.StatusPending}, nil
}

func TestStripeRefundFailedAfterSuccessReconciles(t *testing.T) {
	for _, terminal := range []string{"failed", "canceled"} {
		t.Run(terminal, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment) VALUES($1,'stripe',$2,'test')", f.tenant, account)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OPENSITE_ENVIRONMENT", "test")
			t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
			t.Setenv("STRIPE_TEST_WEBHOOK_SECRET", "whsec_isolated_transition_signing_key")
			t.Setenv("STRIPE_TEST_ACCOUNT_ID", account)
			rules := checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}})
			f.service = checkout.New(checkout.Config{Run: f.run, Cart: f.cart, Rules: rules, Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: stripeFactIntent{}})
			start := f.start(t)
			var at time.Time
			s, err := payments.NewStripe(payments.Config{Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
				return f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
					var tenant string
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
			}, Apply: f.service.(checkoutcontract.PaymentEventApplier).ApplyPaymentEvent, Admit: func(context.Context, payment.CreatePaymentIntent, bool) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at)
			}); err != nil {
				t.Fatal(err)
			}
			deliver := func(id, kind, status string, amount int64) string {
				t.Helper()
				ref, intent := "re_transition", `,"payment_intent":"pi_refundtransition"`
				if kind == "payment_intent.succeeded" {
					ref, intent = "pi_refundtransition", ""
				}
				raw := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"livemode":false,"data":{"object":{"id":%q%s,"status":%q,"amount":%d,"currency":"usd","metadata":{"osc_order":%q,"osc_version":"1"}}}}`, id, kind, ref, intent, status, amount, start.OrderID))
				stamp := fmt.Sprint(at.Unix())
				mac := hmac.New(sha256.New, []byte("whsec_isolated_transition_signing_key"))
				mac.Write([]byte(stamp + "."))
				mac.Write(raw)
				receipt, err := s.Receive(f.ctx, raw, "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
				if err != nil {
					t.Fatal(err)
				}
				return receipt.Status
			}
			deliver("evt_capture", "payment_intent.succeeded", "succeeded", start.AmountMinor)
			before := f.witness(t, start.OrderID)
			deliver("evt_pending", "refund.created", "pending", 1)
			if f.witness(t, start.OrderID) != before {
				t.Fatal("pending refund posted money")
			}
			if deliver("evt_succeeded", "refund.updated", "succeeded", 1) != "processed" {
				t.Fatal("succeeded refund was not journaled")
			}
			posted := f.witness(t, start.OrderID)
			if posted.journals != 2 || posted.cash != start.AmountMinor-1 {
				t.Fatalf("refund was not journaled exactly once: %+v", posted)
			}
			for i := 0; i < 2; i++ {
				if deliver("evt_terminal", "refund.updated", terminal, 1) != "unmatched" {
					t.Fatal("terminal refund transition silently ignored")
				}
			}
			var retained int
			err = f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT count(*) FROM app.webhook_inbox i WHERE tenant_id=$1 AND provider='stripe' AND provider_event_id='evt_terminal' AND status='failed' AND octet_length(payload_hash)=32 AND NOT EXISTS(SELECT FROM app.payment_events e WHERE (e.tenant_id,e.id)=(i.tenant_id,i.id))", f.tenant).Scan(&retained)
			})
			// Owner split: reversal is C-19d. A journaled success remains immutable
			// while terminal provider facts are retained for reconciliation.
			if err != nil || retained != 1 || f.witness(t, start.OrderID) != posted {
				t.Fatalf("terminal failure must retain reconciliation without changing posted money: count=%d err=%v", retained, err)
			}
		})
	}
}

type paymentKeyReadFailure struct{ pgx.Tx }

func (tx paymentKeyReadFailure) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: GetPaymentEventByKey") {
		return tx.Tx.QueryRow(ctx, "SELECT 1/0")
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestPaymentsFactReadFailureIsBounded(t *testing.T) {
	p := newPaymentFactFixture(t)
	m, err := payments.New(payments.Config{Key: p.key, Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
		return p.run(ctx, account, func(ctx context.Context, tx pgx.Tx, tenant string, at time.Time) error {
			return fn(ctx, paymentKeyReadFailure{tx}, tenant, at)
		})
	}, Apply: p.f.service.(checkoutcontract.PaymentEventApplier).ApplyPaymentEvent})
	if err != nil {
		t.Fatal(err)
	}
	raw := p.raw("db-failure", payment.StatusCaptured, p.start.AmountMinor)
	_, err = m.Receive(p.f.ctx, raw, p.signature(raw, p.at))
	var public *apperrors.Error
	if !errors.As(err, &public) || public.Status != 503 || public.Code != "payment_unavailable" || strings.Contains(err.Error(), "division") {
		t.Fatalf("fact read failure must be bounded internal error: %v", err)
	}
	if w := p.witness(t); w.inbox != 0 || w.events != 0 || w.checkout.journals != 0 {
		t.Fatalf("failed fact read persisted partial work: %+v", w)
	}
}

func TestReturnsAndProviderFactHaveOneRefundJournal(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.f.provider.before = nil
	r := payment.CreatePaymentIntent{TenantID: p.f.tenant, OrderID: p.start.OrderID, OrderVersion: p.start.OrderVersion, Amount: money.Money{Amount: p.start.AmountMinor, Currency: p.start.Currency}, IdempotencyKey: "capture-for-return"}
	if _, err := p.f.provider.Capture(p.f.ctx, payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: p.start.Payment.ProviderRef}); err != nil {
		t.Fatal(err)
	}
	p.receive(t, "capture-for-return", payment.StatusCaptured, p.start.AmountMinor)
	var line string
	var version int64
	var accounts returnscontract.RefundAccounts
	err := p.f.runner.InTenant(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT version FROM app.orders WHERE tenant_id=$1 AND id=$2),
		 (SELECT id::text FROM app.order_lines WHERE tenant_id=$1 AND order_id=$2 LIMIT 1),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='revenue'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='tax_payable'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='shipping_revenue'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='cash')`, p.f.tenant, p.start.OrderID).Scan(&version, &line, &accounts.Income, &accounts.Tax, &accounts.Shipping, &accounts.Cash)
	})
	if err != nil {
		t.Fatal(err)
	}
	s := returns.New(returns.Config{Runner: fulfillmentRunner{runner: p.f.runner, role: "service"}, Payments: p.f.provider,
		Captures: fulfillmentCapture{captured: returnscontract.CapturedPayment{TenantID: r.TenantID, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: p.start.Payment.ProviderRef, Paid: r.Amount}},
		Accounts: fulfillmentAccounts{tenant: p.f.tenant, accounts: accounts}, Ledger: payments.RefundJournal{Ledger: ledger.New()}})
	var receipt returnscontract.Return
	err = p.f.run(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		receipt, err = s.Request(ctx, tx, p.f.tenant, returnscontract.RequestInput{OrderID: r.OrderID, ExpectedVersion: version, IdempotencyKey: "single-rma", Kind: returnscontract.UnshippedCancel, Reason: "refund authority regression", Lines: []returnscontract.Line{{OrderLineID: line, Quantity: 1}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	input := returnscontract.RefundInput{ReturnID: receipt.ID, ExpectedVersion: receipt.OrderVersion, IdempotencyKey: "single-money"}
	crash := errors.New("abort after provider success before commit")
	if _, err := s.(returnscontract.RefundCompleter).RefundWithCompletion(p.f.ctx, p.f.tenant, input, func(context.Context, pgx.Tx, returnscontract.Return) error { return crash }); !errors.Is(err, crash) {
		t.Fatalf("provider-success crash was not rolled back: %v", err)
	}
	events := p.f.provider.fake.Events()
	fact := events[len(events)-1]
	var early map[string]any
	_ = json.Unmarshal(p.raw("rma-before-commit", payment.StatusPartiallyRefunded, fact.Amount.Amount), &early)
	early["refund_id"] = fact.RefundID
	raw, _ := json.Marshal(early)
	if earlyReceipt, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil || earlyReceipt.Status == "processed" {
		t.Fatalf("early webhook must remain pending before durable binding: %+v %v", earlyReceipt, err)
	}
	completed, err := s.Refund(p.f.ctx, p.f.tenant, input)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.Refund(p.f.ctx, p.f.tenant, input); err != nil || !reflect.DeepEqual(replay, completed) {
		t.Fatalf("completed RMA replay changed receipt: %+v %v", replay, err)
	}
	for _, delivery := range []string{"rma-created", "rma-updated", "rma-created"} {
		var body map[string]any
		_ = json.Unmarshal(p.raw(delivery, payment.StatusPartiallyRefunded, completed.RefundMinor), &body)
		body["refund_id"] = fact.RefundID
		raw, _ := json.Marshal(body)
		if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
			t.Fatal(err)
		}
	}
	// A missing financial identity must be retained without interpreting it as a new refund.
	var missingBody map[string]any
	_ = json.Unmarshal(p.raw("rma-without-identity", payment.StatusPartiallyRefunded, completed.RefundMinor), &missingBody)
	delete(missingBody, "refund_id")
	missing, _ := json.Marshal(missingBody)
	identityReceipt, identityErr := p.module.Receive(p.f.ctx, missing, p.signature(missing, p.at))
	var quarantined, unidentified int
	if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider_event_id='rma-without-identity' AND status='failed' AND octet_length(payload_hash)=32), (SELECT count(*) FROM app.payment_events e JOIN app.webhook_inbox i ON i.id=e.id AND i.tenant_id=e.tenant_id WHERE i.tenant_id=$1 AND i.provider_event_id='rma-without-identity')", p.f.tenant).Scan(&quarantined, &unidentified)
	}); err != nil {
		t.Fatalf("identity-less refund not durably quarantined: %d %v", quarantined, err)
	}
	// Count both journal identities: the earlier witness counted only order sources
	// and could not observe the separate return journal at all.
	var journals, refunded, bindings int64
	err = p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1),
	  (SELECT refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$2),
	  (SELECT count(*) FROM app.payment_return_refunds WHERE tenant_id=$1 AND return_id=$3)`, p.f.tenant, p.start.OrderID, receipt.ID).Scan(&journals, &refunded, &bindings)
	})
	w := p.f.witness(t, p.start.OrderID)
	if identityErr != nil || identityReceipt.Status != "rejected" || quarantined != 1 || unidentified != 0 {
		t.Errorf("IDENTITYLESS_REFUND_RED: receipt=%+v err=%v quarantined=%d", identityReceipt, identityErr, quarantined)
	}
	if err != nil || journals != 2 || bindings != 1 || refunded != completed.RefundMinor || w.cash != p.start.AmountMinor-completed.RefundMinor {
		t.Fatalf("RETURNS_DOUBLE_POST_RED: journals=%d bindings=%d refunded=%d cash=%d err=%v", journals, bindings, refunded, w.cash, err)
	}
}

func TestMixedRefundComponents(t *testing.T) {
	for _, steps := range [][]int64{{55, 0}, {55, 0, 0}, {0, 55, 0}, {1, 54, 0, 1, 0}} {
		t.Run(fmt.Sprint(steps), func(t *testing.T) {
			p := newPaymentFactFixture(t, func(f *checkoutPGFixture) checkoutcontract.StartInput {
				if len(steps) == 2 {
					f.cart.cart.Lines[0].Quantity = 1
					if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
						_, err := tx.Exec(ctx, "UPDATE app.shipping_rates SET amount=0 WHERE tenant_id=$1", f.tenant)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				input := f.input()
				if len(steps) == 2 {
					input.AcceptedTotal.AmountMinor = 110
				}
				return input
			})
			p.f.provider.before = nil
			r := payment.CreatePaymentIntent{TenantID: p.f.tenant, OrderID: p.start.OrderID, OrderVersion: p.start.OrderVersion, Amount: money.Money{Amount: p.start.AmountMinor, Currency: p.start.Currency}, IdempotencyKey: "mixed-capture"}
			if _, err := p.f.provider.Capture(p.f.ctx, payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: p.start.Payment.ProviderRef}); err != nil {
				t.Fatal(err)
			}
			p.receive(t, "mixed-capture", payment.StatusCaptured, r.Amount.Amount)
			var line string
			var accounts returnscontract.RefundAccounts
			if err := p.f.runner.InTenant(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT (SELECT id::text FROM app.order_lines WHERE tenant_id=$1 AND order_id=$2 LIMIT 1),
				(SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='revenue'),
				(SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='tax_payable'),
				(SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='shipping_revenue'),
				(SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='cash')`, p.f.tenant, r.OrderID).Scan(&line, &accounts.Income, &accounts.Tax, &accounts.Shipping, &accounts.Cash)
			}); err != nil {
				t.Fatal(err)
			}
			s := returns.New(returns.Config{Runner: fulfillmentRunner{runner: p.f.runner, role: "service"}, Payments: p.f.provider,
				Captures: fulfillmentCapture{captured: returnscontract.CapturedPayment{TenantID: r.TenantID, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: p.start.Payment.ProviderRef, Paid: r.Amount}},
				Accounts: fulfillmentAccounts{tenant: p.f.tenant, accounts: accounts}, Ledger: payments.RefundJournal{Ledger: ledger.New()}})
			var total int64
			for i, amount := range steps {
				key := fmt.Sprintf("mixed-%d", i)
				if amount > 0 {
					p.receive(t, key, payment.StatusPartiallyRefunded, amount)
				} else {
					var receipt returnscontract.Return
					if err := p.f.run(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
						var version int64
						if err := tx.QueryRow(ctx, "SELECT version FROM app.orders WHERE tenant_id=$1 AND id=$2", p.f.tenant, r.OrderID).Scan(&version); err != nil {
							return err
						}
						var err error
						receipt, err = s.Request(ctx, tx, p.f.tenant, returnscontract.RequestInput{OrderID: r.OrderID, ExpectedVersion: version, IdempotencyKey: key, Kind: returnscontract.UnshippedCancel, Reason: "mixed allocation", Lines: []returnscontract.Line{{OrderLineID: line, Quantity: 1}}})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					completed, err := s.Refund(p.f.ctx, p.f.tenant, returnscontract.RefundInput{ReturnID: receipt.ID, ExpectedVersion: receipt.OrderVersion, IdempotencyKey: key + "-refund"})
					if err != nil {
						t.Fatalf("MIXED_COMPONENT_RED: %v", err)
					}
					amount = completed.RefundMinor
				}
				total += amount
				var net, tax, shipping int64
				var bounded bool
				if err := p.f.runner.InTenant(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, "SELECT refunded_net_minor,refunded_tax_minor,refunded_shipping_minor,refunded_net_minor BETWEEN 0 AND items_net_minor AND refunded_tax_minor BETWEEN 0 AND tax_minor AND refunded_shipping_minor BETWEEN 0 AND shipping_minor FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2", p.f.tenant, r.OrderID).Scan(&net, &tax, &shipping, &bounded)
				}); err != nil || !bounded || net+tax+shipping != total {
					t.Fatalf("MIXED_COMPONENT_RED: parts=%d/%d/%d total=%d err=%v", net, tax, shipping, total, err)
				}
				if len(steps) == 2 && i == 0 && (net != 50 || tax != 5 || shipping != 0) {
					t.Fatalf("net100/tax10 ordinary55: %d/%d/%d", net, tax, shipping)
				}

			}
		})
	}
}
