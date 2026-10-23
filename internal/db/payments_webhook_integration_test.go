//go:build pgx

// Responsibility: prove signed payment facts, admission and checkout effects commit together.
// Dependencies: real migrated PG, Payments/Checkout public ports and existing checkout fixture.
// Caller: CI pgx race gate. Invariants: account authority, durable replay, no provider commands,
// atomic rollback, deferred refund draining and double-entry conservation.
package db_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	paymentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type paymentFactFixture struct {
	f                *checkoutPGFixture
	module           *payments.Module
	start            checkoutcontract.Result
	account          string
	key              []byte
	at               time.Time
	providerCommands atomic.Int32
}

func newPaymentFactFixture(t *testing.T, configure ...func(*checkoutPGFixture)) *paymentFactFixture {
	t.Helper()
	f := newCheckoutPGFixture(t, 10, 2)
	p := &paymentFactFixture{f: f, account: "acct_" + uuid.NewString(), key: []byte("isolated-payment-fixture-key-32-bytes")}
	err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled) VALUES($1,'fake',$2,'test',true)", f.tenant, p.account); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&p.at)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, apply := range configure {
		apply(f)
	}
	p.start = f.start(t)
	f.provider.before = func(context.Context, paymentfake.Operation) error {
		p.providerCommands.Add(1)
		return errors.New("fact consumer must never issue provider commands")
	}
	p.module = p.newModule(t, nil)
	return p
}

// This runner uses the production mapping function, restricted runtime role and
// transaction-local tenant. It does not accept a tenant supplied by an event.
func (p *paymentFactFixture) run(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
	return p.f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var tenant string
		var at time.Time
		if err := tx.QueryRow(ctx, "SELECT tenant_id::text,at_time FROM app.resolve_fake_payment_account($1)", account).Scan(&tenant, &at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		return fn(ctx, tx, tenant, at)
	})
}

func (p *paymentFactFixture) newModule(t *testing.T, override func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error)) *payments.Module {
	t.Helper()
	apply := override
	if apply == nil {
		applier, ok := p.f.service.(checkoutcontract.PaymentEventApplier)
		if !ok {
			t.Fatal("real checkout does not implement PaymentEventApplier")
		}
		apply = applier.ApplyPaymentEvent
	}
	m, err := payments.New(payments.Config{Key: p.key, Run: p.run, Apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (p *paymentFactFixture) raw(id string, status payment.PaymentStatus, amount int64) []byte {
	raw, err := json.Marshal(map[string]any{"event_id": id, "account_id": p.account, "order_id": p.start.OrderID, "order_version": p.start.OrderVersion, "provider_ref": p.start.Payment.ProviderRef, "status": string(status), "amount_minor": amount, "currency": p.start.Currency, "refund_id": func() string {
		if status == payment.StatusPartiallyRefunded || status == payment.StatusRefunded {
			return id
		}
		return ""
	}()})
	if err != nil {
		panic(err)
	}
	return raw
}

func (p *paymentFactFixture) signature(raw []byte, at time.Time) string {
	stamp := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, p.key)
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	return "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (p *paymentFactFixture) receive(t *testing.T, id string, status payment.PaymentStatus, amount int64) paymentcontract.Receipt {
	t.Helper()
	raw := p.raw(id, status, amount)
	r, err := p.module.Receive(checkoutRequestContext(p.f.ctx), raw, p.signature(raw, p.at))
	if err != nil {
		t.Fatalf("receive %s/%s: %v", id, status, err)
	}
	if r.EventID != id || (r.Status != "pending" && r.Status != "processed") {
		t.Fatalf("receipt %#v", r)
	}
	return r
}

type paymentFactWitness struct {
	inbox, events, applied, refunded int64
	checkout                         checkoutWitness
}

func (p *paymentFactFixture) witness(t *testing.T) paymentFactWitness {
	t.Helper()
	w := paymentFactWitness{checkout: p.f.witness(t, p.start.OrderID)}
	err := p.f.runner.InTenant(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider='fake' AND provider_account_id=$2 AND environment='test'),(SELECT count(*) FROM app.payment_events WHERE tenant_id=$1 AND provider_account_id=$2),(SELECT count(*) FROM app.payment_events WHERE tenant_id=$1 AND provider_account_id=$2 AND applied),(SELECT refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$3)", p.f.tenant, p.account, p.start.OrderID).Scan(&w.inbox, &w.events, &w.applied, &w.refunded)
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.providerCommands.Load() != 0 || p.f.provider.captures.Load() != 0 || p.f.provider.cancels.Load() != 0 {
		t.Fatal("fact consumer issued provider Capture/Cancel/Refund")
	}
	return w
}

func TestPaymentsPGSignatureAccountAndBindingNoWrite(t *testing.T) {
	for _, name := range []string{"bad_signature", "tampered_bytes", "stale", "future", "unknown_account", "disabled_account", "foreign_account", "payload_tenant", "unknown_order", "wrong_amount", "wrong_currency", "wrong_ref", "wrong_version"} {
		t.Run(name, func(t *testing.T) {
			p := newPaymentFactFixture(t)
			before := p.witness(t)
			raw := p.raw("reject:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			stamp := p.at
			var payloadTenant *paymentFactFixture
			var payloadBefore paymentFactWitness
			switch name {
			case "stale":
				stamp = stamp.Add(-time.Hour)
			case "future":
				stamp = stamp.Add(time.Hour)
			case "unknown_account":
				body["account_id"] = "unknown_" + uuid.NewString()
			case "disabled_account", "foreign_account":
				err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
					if name == "disabled_account" {
						_, err := tx.Exec(ctx, "UPDATE app.payment_provider_accounts SET enabled=false WHERE provider_account_id=$1", p.account)
						return err
					}

					var tenant string
					if err := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('payment-foreign-account') RETURNING id::text").Scan(&tenant); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled) VALUES($1,'fake',$2,'test',true)", tenant, "foreign_"+p.account)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if name == "foreign_account" {
					body["account_id"] = "foreign_" + p.account
				}
			case "payload_tenant":
				// A valid other tenant/order removes missing-tenant or invalid-order
				// failures as accidental admission guards. The signed account stays p's.
				payloadTenant = newPaymentFactFixture(t)
				payloadBefore = payloadTenant.witness(t)
				if err := json.Unmarshal(payloadTenant.raw(body["event_id"].(string), payment.StatusCaptured, payloadTenant.start.AmountMinor), &body); err != nil {
					t.Fatal(err)
				}
				body["account_id"] = p.account
				body["tenant_id"] = payloadTenant.f.tenant
			case "unknown_order":
				body["order_id"] = uuid.NewString()
			case "wrong_amount":
				body["amount_minor"] = p.start.AmountMinor + 1
			case "wrong_currency":
				body["currency"] = "EUR"
			case "wrong_ref":
				body["provider_ref"] = "fake_foreign"
			case "wrong_version":
				body["order_version"] = p.start.OrderVersion + 1
			}
			raw, _ = json.Marshal(body)
			sig := p.signature(raw, stamp)
			if name == "bad_signature" {
				sig = "t=" + fmt.Sprint(stamp.Unix()) + ",v1=" + hex.EncodeToString(make([]byte, 32))
			}
			if name == "tampered_bytes" {
				raw = append(raw, ' ')
			}
			_, admissionErr := p.module.Receive(checkoutRequestContext(p.f.ctx), raw, sig)
			if admissionErr == nil {
				t.Fatal("invalid fact accepted")
			}
			if payloadTenant != nil {
				var public *apperrors.Error
				if !errors.As(admissionErr, &public) || public.Status != 400 || public.Code != "invalid_event" {
					t.Fatalf("payload tenant must fail envelope admission, not FK/RLS or reconciliation: %v", admissionErr)
				}
				if after := payloadTenant.witness(t); after != payloadBefore {
					t.Fatalf("payload-selected tenant changed: before %+v after %+v", payloadBefore, after)
				}
			}
			after := p.witness(t)
			rejectedFact := name == "unknown_order" || name == "wrong_amount" || name == "wrong_currency" || name == "wrong_ref" || name == "wrong_version"
			if rejectedFact {
				if after.checkout != before.checkout || after.refunded != 0 || after.inbox != 1 || after.events != 1 || after.applied != 0 {
					t.Fatalf("reconciliation refusal %+v", after)
				}
				var failed int
				if err := p.f.runner.InTenant(p.f.ctx, p.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, "SELECT count(*) FROM app.payment_events e JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(e.tenant_id,e.id) WHERE e.tenant_id=$1 AND i.provider_event_id=$2 AND i.status='failed' AND e.error_code='state_conflict' AND NOT e.applied", p.f.tenant, body["event_id"]).Scan(&failed)
				}); err != nil || failed != 1 {
					t.Fatalf("durable refusal=%d %v", failed, err)
				}
			} else if after != before {
				t.Fatalf("invalid admission wrote state: before %+v after %+v", before, after)
			}
			// Mapped foreign/unknown order claims retain only a failed receipt; no business effects.
			var count int
			expected := 0
			if rejectedFact || name == "foreign_account" {
				expected = 1
			}
			if err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT count(*) FROM app.webhook_inbox WHERE provider_event_id=$1", body["event_id"]).Scan(&count)
			}); err != nil || count != expected {
				t.Fatalf("global inbox=%d expected=%d %v", count, expected, err)
			}
		})
	}
}

func TestPaymentsPGAuthorizedDoesNotPayAndAccountScopedEventIdentity(t *testing.T) {
	a, b := newPaymentFactFixture(t), newPaymentFactFixture(t)
	shared := "same-provider-event:" + uuid.NewString()
	raw := a.raw("authorized:"+uuid.NewString(), payment.StatusAuthorized, a.start.AmountMinor)
	if _, err := a.module.Receive(a.f.ctx, raw, a.signature(raw, a.at)); err == nil {
		t.Fatal("authorization accepted as a completed fact")
	}
	before := a.witness(t)
	if before.checkout.state != "pending_payment" || before.checkout.cash != 0 || before.checkout.journals != 0 || before.checkout.onHand != 10 || before.checkout.reserved != 2 {
		t.Fatalf("authorization was consumed as capture %+v", before)
	}
	a.receive(t, shared, payment.StatusCaptured, a.start.AmountMinor)
	b.receive(t, shared, payment.StatusCaptured, b.start.AmountMinor)
	for _, p := range []*paymentFactFixture{a, b} {
		w := p.witness(t)
		if w.checkout.state != "paid" || w.checkout.cash != p.start.AmountMinor || w.checkout.journals != 1 {
			t.Fatalf("account-scoped same ID %+v", w)
		}
	}
	var rows, accounts int
	if err := a.f.runner.InMaintenance(a.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*),count(DISTINCT provider_account_id) FROM app.webhook_inbox WHERE provider_event_id=$1", shared).Scan(&rows, &accounts)
	}); err != nil || rows != 2 || accounts != 2 {
		t.Fatalf("account dedup rows=%d accounts=%d %v", rows, accounts, err)
	}
}

func TestPaymentsPGCapturedReplayConflictAndLateFailure(t *testing.T) {
	p := newPaymentFactFixture(t)
	id := "capture:" + uuid.NewString()
	if got := p.receive(t, id, payment.StatusCaptured, p.start.AmountMinor); got.Status != "processed" {
		t.Fatalf("capture pending %#v", got)
	}
	w := p.witness(t)
	if w.inbox != 1 || w.events != 1 || w.applied != 1 || w.checkout.state != "paid" || w.checkout.onHand != 8 || w.checkout.reserved != 0 || w.checkout.journals != 1 || w.checkout.debits != w.checkout.credits || w.checkout.cash != p.start.AmountMinor || w.checkout.receivable != 0 || w.checkout.paidEvents != 1 {
		t.Fatalf("capture witness %+v", w)
	}
	for range 3 {
		p.receive(t, id, payment.StatusCaptured, p.start.AmountMinor)
	}
	if after := p.witness(t); after != w {
		t.Fatalf("replay changed effects: %+v %+v", w, after)
	}
	raw := p.raw(id, payment.StatusCaptured, p.start.AmountMinor+1)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("same event different hash accepted")
	}
	if after := p.witness(t); after != w {
		t.Fatal("hash conflict changed receipt/effects")
	}
	raw = p.raw("late-authorized:"+uuid.NewString(), payment.StatusAuthorized, p.start.AmountMinor)
	var public *apperrors.Error
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); !errors.As(err, &public) || public.Status != 400 || public.Code != "invalid_event" {
		t.Fatalf("late authorization should be rejected: %v", err)
	}
	if after := p.witness(t); after != w {
		t.Fatal("rejected late authorization changed receipt/effects")
	}
	for _, status := range []payment.PaymentStatus{payment.StatusFailed, payment.StatusCanceled} {
		p.receive(t, "late:"+uuid.NewString(), status, p.start.AmountMinor)
	}
	after := p.witness(t)
	if after.checkout != w.checkout || after.refunded != 0 {
		t.Fatalf("late facts rewound captured: %+v", after)
	}
}

// A provider failure/cancellation is a terminal fact, not a command to the PSP.
// It releases unpaid inventory once and never books a cash or refund journal.
func TestPaymentsPGUnpaidFailureAndCancellationReleaseOnce(t *testing.T) {
	for _, status := range []payment.PaymentStatus{payment.StatusFailed, payment.StatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			p := newPaymentFactFixture(t)
			id := "unpaid:" + uuid.NewString()
			if got := p.receive(t, id, status, p.start.AmountMinor); got.Status != "processed" {
				t.Fatalf("terminal fact not processed: %+v", got)
			}
			before := p.witness(t)
			if before.inbox != 1 || before.events != 1 || before.applied != 1 || before.refunded != 0 ||
				before.checkout.state != "cancelled" || before.checkout.phase != "failed" ||
				before.checkout.onHand != 10 || before.checkout.reserved != 0 || before.checkout.journals != 0 ||
				before.checkout.cash != 0 || before.checkout.receivable != 0 || before.checkout.paidEvents != 0 || before.checkout.cancelEvents != 1 {
				t.Fatalf("unpaid terminal witness: %+v", before)
			}
			p.receive(t, id, status, p.start.AmountMinor)
			if after := p.witness(t); after != before {
				t.Fatalf("terminal replay changed effects: %+v %+v", before, after)
			}
			raw := p.raw("capture-after-terminal:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
			var public *apperrors.Error
			if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); !errors.As(err, &public) || public.Status != 409 || public.Code != "payment_conflict" {
				t.Fatalf("capture after terminal needs reconciliation: %v", err)
			}
			after := p.witness(t)
			if after.checkout != before.checkout || after.refunded != 0 || after.applied != before.applied || after.inbox != before.inbox+1 || after.events != before.events+1 {
				t.Fatalf("conflicting capture changed business effects or lost its receipt: %+v", after)
			}
		})
	}
}

func TestPaymentsPGConcurrentDuplicate(t *testing.T) {
	p := newPaymentFactFixture(t)
	raw := p.raw("duplicate:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	sig := p.signature(raw, p.at)
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { <-start; _, err := p.module.Receive(checkoutRequestContext(p.f.ctx), raw, sig); results <- err })
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("duplicate delivery: %v", err)
		}
	}
	w := p.witness(t)
	if w.inbox != 1 || w.events != 1 || w.applied != 1 || w.checkout.journals != 1 || w.checkout.paidEvents != 1 || w.checkout.cash != p.start.AmountMinor {
		t.Fatalf("duplicate effects %+v", w)
	}
}

func TestPaymentsPGAdmissionAndEffectsRollbackTogether(t *testing.T) {
	p := newPaymentFactFixture(t)
	applier := p.f.service.(checkoutcontract.PaymentEventApplier)
	injected := errors.New("C19_AFTER_PAYMENT_EFFECTS_ROLLBACK")
	injectedAfterApply := false
	p.module = p.newModule(t, func(ctx context.Context, tx pgx.Tx, event payment.VerifiedEvent) (bool, error) {
		applied, err := applier.ApplyPaymentEvent(ctx, tx, event)
		if err != nil {
			return applied, err
		}
		injectedAfterApply = true
		return applied, injected
	})
	before := p.witness(t)
	raw := p.raw("rollback:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	var public *apperrors.Error
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); !injectedAfterApply || !errors.As(err, &public) || public.Status != 503 || public.Code != "payment_unavailable" || public.Detail != "Payment event processing unavailable" {
		t.Fatalf("rollback must sanitize the injected post-apply error: applied=%v err=%v", injectedAfterApply, err)
	}
	if after := p.witness(t); after != before {
		t.Fatalf("partial commit: %+v %+v", before, after)
	}
	p.module = p.newModule(t, nil)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err != nil {
		t.Fatal("rollback poisoned retry", err)
	}
	if w := p.witness(t); w.inbox != 1 || w.applied != 1 || w.checkout.state != "paid" {
		t.Fatalf("retry %+v", w)
	}
}

func TestPaymentsPGDeferredRefundDrainAndConservation(t *testing.T) {
	p := newPaymentFactFixture(t)
	refund := "refund:" + uuid.NewString()
	if got := p.receive(t, refund, payment.StatusPartiallyRefunded, 70); got.Status != "pending" {
		t.Fatalf("refund before capture %#v", got)
	}
	pending := p.witness(t)
	if pending.inbox != 1 || pending.events != 1 || pending.applied != 0 || pending.checkout.journals != 0 || pending.checkout.cash != 0 || pending.refunded != 0 {
		t.Fatalf("refund not deferred %+v", pending)
	}
	p.receive(t, "capture:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	w := p.witness(t)
	if w.inbox != 2 || w.applied != 2 || w.refunded != 70 || w.checkout.state != "partially_refunded" || w.checkout.cash != p.start.AmountMinor-70 || w.checkout.debits != w.checkout.credits || w.checkout.receivable != 0 {
		t.Fatalf("refund drain %+v", w)
	}
	p.receive(t, refund, payment.StatusPartiallyRefunded, 70)
	if after := p.witness(t); after != w {
		t.Fatal("refund replay double booked")
	}
	p.receive(t, "refund-rest:"+uuid.NewString(), payment.StatusRefunded, p.start.AmountMinor-70)
	full := p.witness(t)
	if full.refunded != p.start.AmountMinor || full.checkout.state != "refunded" || full.checkout.cash != 0 || full.checkout.receivable != 0 || full.checkout.debits != full.checkout.credits || full.checkout.onHand != 8 || full.checkout.reserved != 0 {
		t.Fatalf("full refund conservation %+v", full)
	}
	raw := p.raw("refund-over:"+uuid.NewString(), payment.StatusRefunded, 1)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("over-refund accepted")
	}
	after := p.witness(t)
	if after.checkout != full.checkout || after.refunded != full.refunded || after.applied != full.applied || after.inbox != full.inbox+1 || after.events != full.events+1 {
		t.Fatal("over-refund changed money or lost durable rejected fact")
	}
}

func TestPaymentsPGSignedHTTPAdmissionToLedger(t *testing.T) {
	p := newPaymentFactFixture(t)
	h, err := checkouthttp.New(checkouthttp.Config{FakeEnabled: true, Environment: "test", Events: p.module})
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.New(nil, nil)
	for _, route := range h.Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	post := func(raw []byte, sig string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, checkouthttp.FakePattern, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Fake-Signature", sig)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	raw := p.raw("http:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	before := p.witness(t)
	if w := post(raw, "t=0,v1="+hex.EncodeToString(make([]byte, 32))); w.Code != 401 {
		t.Fatalf("unsigned route=%d %s", w.Code, w.Body.String())
	}
	if after := p.witness(t); after != before {
		t.Fatal("HTTP bad signature wrote state")
	}
	for range 3 {
		if w := post(raw, p.signature(raw, p.at)); w.Code != 200 {
			t.Fatalf("signed route=%d %s", w.Code, w.Body.String())
		}
	}
	w := p.witness(t)
	if w.inbox != 1 || w.events != 1 || w.applied != 1 || w.checkout.state != "paid" || w.checkout.journals != 1 || w.checkout.cash != p.start.AmountMinor || w.checkout.debits != w.checkout.credits || w.checkout.paidEvents != 1 {
		t.Fatalf("signed endpoint→account→inbox→ledger/order %+v", w)
	}
}

// The runner's first DB timestamp can become stale while waiting for the intent
// lock. Supply that old observation deterministically; no sleeps or time races.
func TestPaymentsPGFreshnessRecheckedAfterStreamLock(t *testing.T) {
	p := newPaymentFactFixture(t)
	stamp := p.at.Add(-10 * time.Minute)
	before := p.witness(t)
	m, err := payments.New(payments.Config{Key: p.key,
		Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
			return p.run(ctx, account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
				return fn(ctx, tx, tenant, stamp)
			})
		},
		Apply: func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error) {
			t.Fatal("expired queued fact reached business writes")
			return false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := p.raw("queued-expired:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	if _, err = m.Receive(p.f.ctx, raw, p.signature(raw, stamp)); err == nil {
		t.Fatal("stale initial DB clock bypassed post-lock freshness")
	}
	if after := p.witness(t); after != before {
		t.Fatal("expired queued event left persistent writes")
	}
}

func TestPaymentsPGRejectedRefundDoesNotPoisonCapture(t *testing.T) {
	p := newPaymentFactFixture(t)
	raw := p.raw("invalid-refund:"+uuid.NewString(), payment.StatusPartiallyRefunded, 1)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["currency"] = "EUR"
	raw, _ = json.Marshal(body)
	if _, err := p.module.Receive(p.f.ctx, raw, p.signature(raw, p.at)); err == nil {
		t.Fatal("wrong currency refund accepted")
	}
	p.receive(t, "capture-after-rejected:"+uuid.NewString(), payment.StatusCaptured, p.start.AmountMinor)
	w := p.witness(t)
	if w.inbox != 2 || w.events != 2 || w.applied != 1 || w.refunded != 0 || w.checkout.state != "paid" || w.checkout.journals != 1 || w.checkout.cash != p.start.AmountMinor {
		t.Fatalf("rejected peer poisoned capture: %+v", w)
	}
}
