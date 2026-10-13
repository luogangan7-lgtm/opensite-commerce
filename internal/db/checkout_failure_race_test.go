//go:build pgx

// Responsibility: reject inconsistent Fake outcomes during concurrent retries.
// Dependencies: existing checkout PG fixture, public module contracts and Fake.
// Caller: the real PostgreSQL integration gate. Channels order provider returns,
// with no sleep; every permanent rejection must retain its same-key outcome.
package db_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
)

// The first provider call completes with an injected permanent rejection before
// the second call executes. Both responses are parked after provider execution;
// the first response returns to checkout before the second response is released.
// This admits the identical persisted claim twice without relying on scheduling.
type checkoutFailureRacePort struct {
	payment.Port
	op                          paymentfake.Operation
	calls                       atomic.Int32
	first, second               chan struct{}
	releaseFirst, releaseSecond chan struct{}
}

func newCheckoutFailureRacePort(p payment.Port, op paymentfake.Operation) *checkoutFailureRacePort {
	return &checkoutFailureRacePort{Port: p, op: op, first: make(chan struct{}), second: make(chan struct{}), releaseFirst: make(chan struct{}), releaseSecond: make(chan struct{})}
}
func (p *checkoutFailureRacePort) park(ctx context.Context, n int32) error {
	var wait <-chan struct{}
	switch n {
	case 1:
		close(p.first)
		wait = p.releaseFirst
	case 2:
		close(p.second)
		wait = p.releaseSecond
	default:
		return nil
	}
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *checkoutFailureRacePort) CreateIntent(ctx context.Context, in payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	if p.op != paymentfake.Create {
		return p.Port.CreateIntent(ctx, in)
	}
	n := p.calls.Add(1)
	result, err := p.Port.CreateIntent(ctx, in)
	if parked := p.park(ctx, n); parked != nil {
		return result, parked
	}
	return result, err
}
func (p *checkoutFailureRacePort) Capture(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if p.op != paymentfake.Capture {
		return p.Port.Capture(ctx, in)
	}
	n := p.calls.Add(1)
	result, err := p.Port.Capture(ctx, in)
	if parked := p.park(ctx, n); parked != nil {
		return result, parked
	}
	return result, err
}
func checkoutFailureRaceService(f *checkoutPGFixture, p payment.Port) checkoutcontract.Service {
	rules := checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}})
	return checkout.New(checkout.Config{Run: f.run, Cart: f.cart, Rules: rules, Orders: orders.New(), Ledger: ledger.New(), Payment: p})
}
func checkoutFailureRaceAwait(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("provider race synchronization timed out", ctx.Err())
	}
}

type checkoutFailureRaceResponse struct {
	result checkoutcontract.Result
	err    error
}

func checkoutFailureRaceReceive(t *testing.T, ctx context.Context, ch <-chan checkoutFailureRaceResponse) checkoutFailureRaceResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		t.Fatal("checkout race response timed out", ctx.Err())
		return checkoutFailureRaceResponse{}
	}
}
func checkoutFailureRaceRun(t *testing.T, f *checkoutPGFixture, p *checkoutFailureRacePort, call func() (checkoutcontract.Result, error)) (checkoutFailureRaceResponse, checkoutFailureRaceResponse) {
	t.Helper()
	first, second := make(chan checkoutFailureRaceResponse, 1), make(chan checkoutFailureRaceResponse, 1)
	go func() { r, e := call(); first <- checkoutFailureRaceResponse{r, e} }()
	checkoutFailureRaceAwait(t, f.ctx, p.first)
	go func() { r, e := call(); second <- checkoutFailureRaceResponse{r, e} }()
	checkoutFailureRaceAwait(t, f.ctx, p.second)
	close(p.releaseFirst)
	a := checkoutFailureRaceReceive(t, f.ctx, first)
	close(p.releaseSecond)
	b := checkoutFailureRaceReceive(t, f.ctx, second)
	return a, b
}

func TestCheckoutPGConcurrentPermanentCaptureFailure(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	start := f.start(t)
	event := f.event(start, payment.StatusAuthorized)
	p := newCheckoutFailureRacePort(f.provider, paymentfake.Capture)
	svc := checkoutFailureRaceService(f, p)
	f.provider.fake.FailNext(paymentfake.Capture, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "definitive pre-effect rejection"})
	first, second := checkoutFailureRaceRun(t, f, p, func() (checkoutcontract.Result, error) { return svc.Confirm(checkoutRequestContext(f.ctx), event) })
	replay, retryErr := svc.Confirm(checkoutRequestContext(f.ctx), event)
	w := f.witness(t, start.OrderID)
	var captures int
	for _, e := range f.provider.fake.Events() {
		if e.Status == payment.StatusCaptured {
			captures++
		}
	}
	t.Logf("first=%v second=%v retry=%v phase=%s order=%s reserved=%d journals=%d capture_events=%d", first.err, second.err, retryErr, w.phase, w.state, w.reserved, w.journals, captures)
	if captures != 0 || w.phase != "failed" || w.state != "cancelled" || w.reserved != 0 || w.onHand != 10 || w.journals != 0 || w.cancelEvents != 1 || w.paidEvents != 0 {
		t.Fatalf("permanent rejection changed on retry or lost recoverable terminal result: %#v capture_events=%d", w, captures)
	}
	if first.err != nil || retryErr != nil || first.result.State != "failed" || replay != first.result {
		t.Fatalf("failed checkout result did not replay: first=%#v replay=%#v retry=%v", first, replay, retryErr)
	}
}

func TestCheckoutPGConcurrentPermanentCreateFailure(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 2)
	p := newCheckoutFailureRacePort(f.provider, paymentfake.Create)
	svc := checkoutFailureRaceService(f, p)
	in := f.input()
	f.provider.fake.FailNext(paymentfake.Create, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "definitive pre-effect rejection"})
	first, second := checkoutFailureRaceRun(t, f, p, func() (checkoutcontract.Result, error) { return svc.Start(checkoutRequestContext(f.ctx), f.actor, in) })
	replay, retryErr := svc.Start(checkoutRequestContext(f.ctx), f.actor, in)
	var oid string
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT order_id::text FROM app.checkout_sessions WHERE tenant_id=$1 AND actor_kind=$2 AND actor_id=$3 AND request_key=$4", f.tenant, f.actor.Kind(), f.actor.ID(), in.IdempotencyKey).Scan(&oid)
	}); err != nil {
		t.Fatal(err)
	}
	w := f.witness(t, oid)
	events := f.provider.fake.Events()
	t.Logf("first=%v second=%v retry=%v phase=%s order=%s reserved=%d journals=%d provider_events=%d", first.err, second.err, retryErr, w.phase, w.state, w.reserved, w.journals, len(events))
	if len(events) != 0 || w.phase != "failed" || w.state != "cancelled" || w.reserved != 0 || w.onHand != 10 || w.journals != 0 || w.cancelEvents != 1 || w.paidEvents != 0 {
		t.Fatalf("permanent create rejection left orphan provider intent or inconsistent result: %#v events=%#v", w, events)
	}
	if first.err != nil || second.err != nil || retryErr != nil || first.result.State != "failed" || first.result != second.result || replay != first.result {
		t.Fatalf("same-key failure did not replay: first=%#v second=%#v replay=%#v retry=%v", first, second, replay, retryErr)
	}
}
