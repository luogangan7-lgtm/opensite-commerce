//go:build pgx && stripe_sandbox

// Responsibility: real Stripe TEST API commands plus CLI-forwarded signed events.
// Dependencies: disposable PG fixture, production Stripe port/mapped runner and
// Checkout/Orders/Returns/Ledger contracts. Caller: optional stripe-sandbox CI only.
// Invariants: durable consent, one event application, bounded failures, balanced
// payment/refund journals. Event bodies/signatures stay only in process memory.
package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	ch "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	fc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	rc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type stripeDelivery struct {
	id, kind, signature, order, refund string
	raw                                []byte
	status                             int
}

func TestStripeSandboxLifecycle(t *testing.T) {
	if !strings.HasPrefix(os.Getenv("STRIPE_TEST_SECRET_KEY"), "sk_test_") {
		t.Fatal("FAIL: no test secret; this explicit CI gate cannot skip")
	}
	f := newCheckoutPGFixture(t, 100, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
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
			TerminalRun: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
				return runner.RunStripePaymentTx(ctx, account, func(ctx context.Context, tx pgx.Tx, tenant string, at time.Time) error {
					if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_payments_ingress"); err != nil {
						return err
					}
					return fn(ctx, tx, tenant, at)
				})
			},
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
		svc = checkout.New(checkout.Config{Run: f.run, Cart: f.cart, Rules: rules, Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: adapter})
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
					ID       string            `json:"id"`
					Metadata map[string]string `json:"metadata"`
				} `json:"object"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &event)
		select {
		case deliveries <- stripeDelivery{id: event.ID, kind: event.Type, signature: r.Header.Get("Stripe-Signature"), order: event.Data.Object.Metadata["osc_order"], refund: event.Data.Object.ID, raw: raw, status: rec.Code}:
		default:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	seen := map[string]bool{}
	expectedRefunds := map[string]string{}
	pending := []stripeDelivery{}
	var timeoutDiagnostic func()
	wait := func(kind, order string) stripeDelivery {
		t.Helper()
		limit := 30 * time.Second
		if kind == "refund.failed" {
			limit = 4 * time.Minute
		}
		timer := time.NewTimer(limit)
		defer timer.Stop()
		observed := map[string]int{}
		for {
			for i, d := range pending {
				if d.kind == kind && (d.order == order || (expectedRefunds[order] != "" && expectedRefunds[order] == d.refund)) && !seen[d.id] {
					pending = append(pending[:i], pending[i+1:]...)
					seen[d.id] = true
					if d.status != 200 && !(kind == "payment_intent.payment_failed" && d.status == 409) {
						t.Fatalf("signed %s admission status=%d", kind, d.status)
					}
					return d
				}
			}
			select {
			case d := <-deliveries:
				observed[fmt.Sprintf("%s/http%d", d.kind, d.status)]++
				pending = append(pending, d)
			case <-timer.C:
				if timeoutDiagnostic != nil {
					timeoutDiagnostic()
				}
				t.Fatalf("CLI delivery timeout: %s; observed=%v pending=%d", kind, observed, len(pending))
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
	// Use the same persisted Payments readers and closed coordinator commands as
	// bootstrap; no caller-supplied amount, capture or account fixture is trusted.
	journal := payments.RefundJournal{Ledger: ledger.New()}
	coordinator := returns.New(returns.Config{Runner: fulfillmentRunner{runner: f.runner, role: "service"}, Payments: adapter,
		Captures: sandboxReturnReaders{journal}, Accounts: sandboxReturnReaders{journal}, Ledger: journal})
	var line, shipmentLine string
	version := func() int64 {
		t.Helper()
		var v int64
		if err := f.runner.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT version FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, start.OrderID).Scan(&v)
		}); err != nil {
			t.Fatal(err)
		}
		return v
	}
	err = f.run(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT id::text FROM app.order_lines WHERE tenant_id=$1 AND order_id=$2", f.tenant, start.OrderID).Scan(&line); err != nil {
			return err
		}
		shipment, err := fulfillment.New().CreateShipment(ctx, tx, f.tenant, fc.CreateInput{OrderID: start.OrderID, ExpectedVersion: version(), IdempotencyKey: "sandbox-ship", Carrier: "manual", TrackingNumber: "sandbox", Lines: []fc.Line{{OrderLineID: line, Quantity: 10}}})
		if err == nil {
			shipmentLine = shipment.Lines[0].ID
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefunds := func(amount int64, count int) {
		t.Helper()
		var bindings, receipts, journals, unbalanced, facts int
		var refunded, parts, returned int64
		err := f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT
			 (SELECT count(*) FROM app.payment_return_refunds WHERE tenant_id=$1),
			 (SELECT count(*) FROM app.returns r JOIN app.payment_return_refunds b ON (b.tenant_id,b.return_id,b.journal_id)=(r.tenant_id,r.id,r.ledger_transaction_id) WHERE r.tenant_id=$1 AND r.status='refunded' AND r.kind='shipment_return'),
			 (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND status='posted'),
			 (SELECT count(*) FROM (SELECT x.id FROM app.ledger_transactions x LEFT JOIN app.ledger_entries e ON (e.tenant_id,e.transaction_id)=(x.tenant_id,x.id) WHERE x.tenant_id=$1 GROUP BY x.id HAVING coalesce(sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END),0)<>0) unbalanced),
			 (SELECT count(*) FROM app.payment_events e JOIN app.payment_return_refunds b ON (b.tenant_id,b.provider_ref,b.provider_refund_id)=(e.tenant_id,e.provider_ref,e.provider_refund_id) JOIN app.returns r ON (r.tenant_id,r.id)=(b.tenant_id,b.return_id) WHERE e.tenant_id=$1 AND e.provider='stripe' AND e.status='partially_refunded' AND e.applied AND e.amount_minor=r.refund_minor AND e.currency=r.currency AND e.order_version=r.payment_version AND e.claimed_order_id=r.order_id),
			 (SELECT refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$2),
			 (SELECT refunded_net_minor+refunded_tax_minor+refunded_shipping_minor FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2),
			 (SELECT coalesce(sum(refund_minor),0)::bigint FROM app.returns WHERE tenant_id=$1 AND status='refunded')`, f.tenant, start.OrderID).Scan(&bindings, &receipts, &journals, &unbalanced, &facts, &refunded, &parts, &returned)
		})
		w := f.witness(t, start.OrderID)
		state := "partially_refunded"
		if amount == start.AmountMinor {
			state = "refunded"
		}
		if err != nil || bindings != count || receipts != count || journals != count+1 || unbalanced != 0 || facts != count || refunded != amount || parts != amount || returned != amount || w.cash != start.AmountMinor-amount || w.state != state {
			t.Fatalf("RETURNS_SANDBOX_RED: bindings=%d receipts=%d journals=%d unbalanced=%d facts=%d orders/checkout/returns=%d/%d/%d witness=%+v err=%v", bindings, receipts, journals, unbalanced, facts, refunded, parts, returned, w, err)
		}
	}
	var refunds []stripeDelivery
	var total int64
	for i, quantity := range []int64{3, 7} {
		key := fmt.Sprintf("sandbox-rma-%d", i)
		var rma rc.Return
		err = f.run(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rma, err = coordinator.Request(ctx, tx, f.tenant, rc.RequestInput{OrderID: start.OrderID, ExpectedVersion: version(), IdempotencyKey: key, Kind: rc.ShipmentReturn, Reason: "sandbox return", Lines: []rc.Line{{OrderLineID: line, ShipmentLineID: shipmentLine, Quantity: quantity}}})
			return err
		})
		if err != nil || rma.Status != "requested" {
			t.Fatalf("RMA request: %+v %v", rma, err)
		}
		unreceived := f.witness(t, start.OrderID)
		if _, err := coordinator.Refund(ctx, f.tenant, rc.RefundInput{ReturnID: rma.ID, ExpectedVersion: rma.OrderVersion, IdempotencyKey: key + "-too-early"}); !errors.Is(err, rc.ErrConflict) {
			t.Fatalf("unreceived RMA admitted refund: %v", err)
		}
		if f.witness(t, start.OrderID) != unreceived {
			t.Fatal("unreceived RMA moved money")
		}
		err = f.run(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rma, err = coordinator.Receive(ctx, tx, f.tenant, rc.ReceiveInput{ReturnID: rma.ID, ExpectedVersion: version(), IdempotencyKey: key + "-receive", Restock: false})
			return err
		})
		if err != nil || rma.Status != "received" {
			t.Fatalf("RMA receive: %+v %v", rma, err)
		}
		in := rc.RefundInput{ReturnID: rma.ID, ExpectedVersion: rma.OrderVersion, IdempotencyKey: key + "-refund"}
		completed, err := coordinator.Refund(ctx, f.tenant, in)
		if err != nil || completed.Status != "refunded" || completed.RefundMinor <= 0 {
			t.Fatalf("coordinator refund: %+v %v", completed, err)
		}
		if replay, err := coordinator.Refund(ctx, f.tenant, in); err != nil || !reflect.DeepEqual(replay, completed) {
			t.Fatalf("coordinator replay: %+v %v", replay, err)
		}
		refund := wait("refund.created", start.OrderID)
		// A CLI delivery may beat coordinator finalization. Exact replay converges
		// its pending receipt after the persisted provider-ID binding commits.
		if post(refund, refund.raw) != 200 {
			t.Fatal("refund duplicate rejected")
		}
		assertEvent(refund.id)
		refunds = append(refunds, refund)
		total += completed.RefundMinor
		if i == 0 && total >= start.AmountMinor {
			t.Fatal("first refund must be partial")
		}
		assertRefunds(total, i+1)
	}
	if total != start.AmountMinor {
		t.Fatalf("full refund total=%d want=%d", total, start.AmountMinor)
	}
	// Replay the old capture and partial refund after the final refund, newest
	// first: signed provider facts must not regress state or post more money.
	for _, d := range []stripeDelivery{refunds[1], refunds[0], capture, refunds[0]} {
		if post(d, d.raw) != 200 {
			t.Fatal("out-of-order delivery rejected")
		}
		assertEvent(d.id)
	}
	assertRefunds(total, 2)
	over := payment.PaymentOperation{CreatePaymentIntent: payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: start.OrderID, OrderVersion: start.OrderVersion, Amount: money.Money{Amount: 1, Currency: start.Currency}, IdempotencyKey: "sandbox-over-refund"}, ProviderRef: start.Payment.ProviderRef}
	if _, err := adapter.Refund(ctx, over); err == nil {
		t.Fatal("Stripe over-refund admitted")
	}
	// The port bounds all provider errors; require a real Stripe 400 as well so
	// an unrelated network failure cannot satisfy the over-refund assertion.
	form := url.Values{"payment_intent": {start.Payment.ProviderRef}, "amount": {"1"}}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.stripe.com/v1/refunds", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal("over-refund request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("STRIPE_TEST_SECRET_KEY"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", "sandbox-over-refund-"+f.tenant)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("over-refund rejection transport failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("Stripe over-refund status=%d want=400", response.StatusCode)
	}
	err = f.run(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := coordinator.Request(ctx, tx, f.tenant, rc.RequestInput{OrderID: start.OrderID, ExpectedVersion: version(), IdempotencyKey: "sandbox-over-rma", Kind: rc.ShipmentReturn, Reason: "exhausted quantity", Lines: []rc.Line{{OrderLineID: line, ShipmentLineID: shipmentLine, Quantity: 1}}})
		return err
	})
	if !errors.Is(err, rc.ErrConflict) {
		t.Fatalf("Returns over-refund admission: %v", err)
	}
	assertRefunds(total, 2)
	// Stripe documents pm_card_refundFail as succeeded -> failed. Both facts
	// below are real CLI-forwarded signatures; the finalizer retrieves the real
	// current object. No fixture signs or fabricates a terminal provider fact.
	failSvc, failAdapter := compose("pm_card_refundFail")
	f.service = failSvc
	failStart := f.start(t)
	if paid, err := failSvc.Confirm(checkoutRequestContext(ctx), f.event(failStart, payment.StatusAuthorized)); err != nil || paid.State != "paid" {
		t.Fatalf("C19D2_SANDBOX_CAPTURE_RED: %v", err)
	}
	wait("payment_intent.succeeded", failStart.OrderID)
	failBefore := f.witness(t, failStart.OrderID)
	op := payment.PaymentOperation{CreatePaymentIntent: payment.CreatePaymentIntent{TenantID: f.tenant,
		OrderID: failStart.OrderID, OrderVersion: failStart.OrderVersion, Amount: money.Money{Amount: 1, Currency: failStart.Currency},
		IdempotencyKey: "sandbox-posted-failure"}, ProviderRef: failStart.Payment.ProviderRef}
	if _, err := failAdapter.Refund(ctx, op); err != nil {
		t.Fatalf("C19D2_SANDBOX_SUCCESS_RED: %v", err)
	}
	created := wait("refund.created", failStart.OrderID)
	assertEvent(created.id)
	if w := f.witness(t, failStart.OrderID); w.cash != failBefore.cash-1 || w.state != "partially_refunded" {
		t.Fatalf("C19D2_SANDBOX_JOURNALED_SUCCESS_RED: %+v", w)
	}
	expectedRefunds[failStart.OrderID] = created.refund
	// Diagnose only the real provider status on a bounded delivery failure.
	// Never retain/log response bodies, signatures, API keys or customer fields.
	timeoutDiagnostic = func() {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.stripe.com/v1/refunds/"+url.PathEscape(created.refund), nil)
		if err != nil {
			t.Log("refund status diagnostic request unavailable")
			return
		}
		req.Header.Set("Authorization", "Bearer "+os.Getenv("STRIPE_TEST_SECRET_KEY"))
		res, err := client.Do(req)
		if err != nil {
			t.Log("refund status diagnostic transport unavailable")
			return
		}
		defer res.Body.Close()
		var current struct {
			Status   string `json:"status"`
			Livemode bool   `json:"livemode"`
		}
		if json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&current) != nil {
			t.Log("refund status diagnostic decode unavailable")
			return
		}
		t.Logf("real refund diagnostic: http=%d status=%q live=%t", res.StatusCode, current.Status, current.Livemode)
	}
	failed := wait("refund.failed", failStart.OrderID)
	timeoutDiagnostic = nil
	compensated := f.witness(t, failStart.OrderID)
	var reversals, inbox int
	var refunded, components int64
	err = f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1),
 (SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=$1 AND provider_event_id=$3 AND status='processed'),
 o.refunded_minor,c.refunded_net_minor+c.refunded_tax_minor+c.refunded_shipping_minor
 FROM app.orders o JOIN app.checkout_sessions c ON (c.tenant_id,c.order_id)=(o.tenant_id,o.id)
 WHERE o.tenant_id=$1 AND o.id=$2`, f.tenant, failStart.OrderID, failed.id).Scan(&reversals, &inbox, &refunded, &components)
	})
	if err != nil || reversals != 1 || inbox != 1 || refunded != 0 || components != 0 || compensated.cash != failBefore.cash || compensated.state != "paid" || compensated.journals != failBefore.journals+1 {
		t.Fatalf("C19D2_SANDBOX_COMPENSATION_RED: reversals=%d inbox=%d net=%d components=%d witness=%+v err=%v", reversals, inbox, refunded, components, compensated, err)
	}
	for _, d := range []stripeDelivery{failed, created, failed} {
		if post(d, d.raw) != 200 {
			t.Fatal("C19D2_SANDBOX_REPLAY_RED")
		}
	}
	if f.witness(t, failStart.OrderID) != compensated {
		t.Fatal("C19D2_SANDBOX_REPLAY_MONEY_RED")
	}
	t.Log("PASS C19D2: real TEST succeeded -> failed refund, one posted reversal and capacity restoration; signed duplicate/stale-success replay inert")
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
	t.Log("PASS: TEST MODE pay/decline/cancel, received Returns partial+full refund, over-refund rejection, duplicate/out-of-order signed replay; Orders/Returns/Payments/Ledger agree")
}

// Persisted readers mirror bootstrap's consumer adapters over Payments ports.
type sandboxReturnReaders struct{ journal payments.RefundJournal }

func (r sandboxReturnReaders) CapturedPayment(ctx context.Context, tx pgx.Tx, tenant, order string) (rc.CapturedPayment, error) {
	c, err := r.journal.PaidContext(ctx, tx, order)
	return rc.CapturedPayment{TenantID: tenant, OrderID: order, ProviderRef: c.ProviderRef, OrderVersion: c.PaymentVersion, Paid: money.Money{Amount: c.AmountMinor, Currency: c.Currency}}, err
}
func (r sandboxReturnReaders) RefundAccounts(ctx context.Context, tx pgx.Tx, tenant, currency string) (rc.RefundAccounts, error) {
	c, err := r.journal.RefundAccounts(ctx, tx, currency)
	return rc.RefundAccounts{Income: c.Accounts["revenue"], Tax: c.Accounts["tax_payable"], Shipping: c.Accounts["shipping_revenue"], Cash: c.Accounts["cash"]}, err
}
