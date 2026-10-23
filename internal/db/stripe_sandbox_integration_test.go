//go:build pgx && stripe_sandbox

// Responsibility: real Stripe TEST API commands plus CLI-forwarded signed events.
// Dependencies: disposable PG fixture, production Stripe port/mapped runner and
// Checkout/Orders/Ledger contracts. Caller: optional stripe-sandbox CI only.
// Invariants: durable consent, one event application, bounded failures, balanced
// payment/refund journals. Event bodies/signatures stay only in process memory.
package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	ch "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type stripeDelivery struct {
	id, kind, signature, order string
	raw                        []byte
	status                     int
}

func TestStripeSandboxLifecycle(t *testing.T) {
	if !strings.HasPrefix(os.Getenv("STRIPE_TEST_SECRET_KEY"), "sk_test_") {
		t.Fatal("FAIL: no test secret; this explicit CI gate cannot skip")
	}
	f := newCheckoutPGFixture(t, 100, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f.ctx = ctx
	runner, err := pgxadapter.OpenQueryRunner(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	err = f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment) VALUES($1,'stripe',$2,'test')", f.tenant, os.Getenv("STRIPE_TEST_ACCOUNT_ID"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	compose := func(method string) (ch.Service, *payments.Stripe) {
		t.Setenv("STRIPE_TEST_PAYMENT_METHOD", method)
		var svc ch.Service
		adapter, err := payments.NewStripe(payments.Config{Run: runner.RunStripePaymentTx,
			Apply: func(ctx context.Context, tx pgx.Tx, e payment.VerifiedEvent) (bool, error) {
				return svc.(ch.PaymentEventApplier).ApplyPaymentEvent(ctx, tx, e)
			},
			Admit: func(ctx context.Context, r payment.CreatePaymentIntent, capture bool) error {
				return svc.(interface {
					AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error
				}).AdmitRealPayment(ctx, r, capture)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		rules := checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}})
		svc = checkout.New(checkout.Config{Run: f.run, Cart: f.cart, Rules: rules, Orders: orders.New(), Ledger: ledger.New(), Payment: adapter})
		return svc, adapter
	}
	svc, adapter := compose("pm_card_visa")
	f.service = svc
	deliveries := make(chan stripeDelivery, 64)
	route := payments.StripeRoute(adapter)
	listener, err := net.Listen("tcp", "127.0.0.1:8787")
	if err != nil {
		t.Fatal("sandbox receiver bind failed")
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != route.Spec.Pattern {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(raw) > 65536 {
			w.WriteHeader(413)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		route.Handler.ServeHTTP(rec, r)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		var event struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data struct {
				Object struct {
					Metadata map[string]string `json:"metadata"`
				} `json:"object"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &event)
		select {
		case deliveries <- stripeDelivery{id: event.ID, kind: event.Type, signature: r.Header.Get("Stripe-Signature"), order: event.Data.Object.Metadata["osc_order"], raw: raw, status: rec.Code}:
		default:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	seen := map[string]bool{}
	wait := func(kind, order string) stripeDelivery {
		t.Helper()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case d := <-deliveries:
				if d.kind == kind && d.order == order && !seen[d.id] {
					seen[d.id] = true
					if d.status != 200 {
						t.Fatalf("signed %s admission status=%d", kind, d.status)
					}
					return d
				}
			case <-timer.C:
				t.Fatalf("CLI delivery timeout: %s", kind)
				return stripeDelivery{}
			}
		}
	}
	post := func(d stripeDelivery, raw []byte) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8787"+route.Spec.Pattern, bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", d.signature)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("duplicate delivery transport failed")
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	assertEvent := func(id string) {
		t.Helper()
		var inbox, events, applied int
		err := f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*),count(e.id),count(*) FILTER(WHERE e.applied) FROM app.webhook_inbox i LEFT JOIN app.payment_events e ON (e.tenant_id,e.id)=(i.tenant_id,i.id) WHERE i.tenant_id=$1 AND i.provider='stripe' AND i.provider_event_id=$2`, f.tenant, id).Scan(&inbox, &events, &applied)
		})
		if err != nil || inbox != 1 || events != 1 || applied != 1 {
			t.Fatalf("event persistence inbox=%d events=%d applied=%d error=%v", inbox, events, applied, err)
		}
	}
	start := f.start(t)
	if start.State != "ready" {
		t.Fatal("intent did not persist ready checkout")
	}
	paid, err := svc.Confirm(checkoutRequestContext(ctx), f.event(start, payment.StatusAuthorized))
	if err != nil || paid.State != "paid" {
		t.Fatalf("pay failed: %v", err)
	}
	capture := wait("payment_intent.succeeded", start.OrderID)
	assertEvent(capture.id)
	before := f.witness(t, start.OrderID)
	if before.cash != start.AmountMinor || before.journals != 1 || before.debits != before.credits || before.state != "paid" {
		t.Fatalf("payment conservation %+v", before)
	}
	if post(capture, capture.raw) != 200 {
		t.Fatal("exact duplicate rejected")
	}
	assertEvent(capture.id)
	if post(capture, append(append([]byte{}, capture.raw...), ' ')) != 401 {
		t.Fatal("signature fault probe: mutated exact bytes admitted")
	}
	if f.witness(t, start.OrderID) != before {
		t.Fatal("duplicate delivery changed financial rows")
	}
	// Stable create/capture identities must survive client retries without a new PSP charge.
	replay, err := svc.Confirm(checkoutRequestContext(ctx), f.event(start, payment.StatusAuthorized))
	if err != nil || replay != paid {
		t.Fatalf("capture replay failed: %v", err)
	}
	for i, amount := range []int64{300, start.AmountMinor - 300} {
		in := payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: start.OrderID, OrderVersion: start.OrderVersion, Amount: money.Money{Amount: amount, Currency: start.Currency}, IdempotencyKey: []string{"sandbox-partial", "sandbox-final"}[i]}
		op := payment.PaymentOperation{CreatePaymentIntent: in, ProviderRef: start.Payment.ProviderRef}
		result, err := adapter.Refund(ctx, op)
		if err != nil {
			t.Fatalf("refund failed: %v", err)
		}
		want := payment.StatusPartiallyRefunded
		if i == 1 {
			want = payment.StatusRefunded
		}
		if result.Status != want {
			t.Fatal("refund provider state mismatch")
		}
		refund := wait("refund.created", start.OrderID)
		assertEvent(refund.id)
		if _, err := adapter.Refund(ctx, op); err != nil {
			t.Fatalf("stable refund replay failed: %v", err)
		}
		if post(refund, refund.raw) != 200 {
			t.Fatal("refund duplicate rejected")
		}
		assertEvent(refund.id)
	}
	refunded := f.witness(t, start.OrderID)
	if refunded.state != "refunded" || refunded.journals != 3 || refunded.debits != refunded.credits || refunded.cash != 0 {
		t.Fatalf("refund conservation %+v", refunded)
	}
	var refundedRows int64
	err = f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM app.payment_events WHERE tenant_id=$1 AND provider='stripe' AND status='partially_refunded' AND applied", f.tenant).Scan(&refundedRows)
	})
	if err != nil || refundedRows != 2 {
		t.Fatal("persisted refund facts missing")
	}
	cancelStart := f.start(t)
	canceled, err := svc.Cancel(checkoutRequestContext(ctx), f.actor, ch.CancelInput{CheckoutID: cancelStart.CheckoutID, IdempotencyKey: "sandbox-cancel"})
	if err != nil || canceled.State != "cancelled" {
		t.Fatalf("cancel failed: %v", err)
	}
	canceledEvent := wait("payment_intent.canceled", cancelStart.OrderID)
	assertEvent(canceledEvent.id)
	if w := f.witness(t, cancelStart.OrderID); w.journals != 0 || w.state != "cancelled" {
		t.Fatal("cancel posted money")
	}
	declineSvc, _ := compose("pm_card_chargeDeclined")
	f.service = declineSvc
	declineStart := f.start(t)
	declined, declineErr := declineSvc.Confirm(checkoutRequestContext(ctx), f.event(declineStart, payment.StatusAuthorized))
	if declineErr != nil || declined.State != "failed" {
		t.Fatalf("decline did not finish failed: %v", declineErr)
	}
	failedEvent := wait("payment_intent.payment_failed", declineStart.OrderID)
	if post(failedEvent, failedEvent.raw) != 200 {
		t.Fatal("failed fact replay rejected")
	}
	assertEvent(failedEvent.id)
	if w := f.witness(t, declineStart.OrderID); w.journals != 0 || w.state == "paid" {
		t.Fatal("decline posted money")
	}
	t.Log("PASS: TEST MODE pay/decline/cancel/partial+full refund, exact webhook replay and persisted balanced journals")
}
