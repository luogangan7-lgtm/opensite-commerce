// Tests exercise the public Payment Port and fake consumer events; no provider IO.
package paymentfake_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
)

func request() payment.CreatePaymentIntent {
	return payment.CreatePaymentIntent{TenantID: "00000000-0000-4000-8000-000000000001", OrderID: "00000000-0000-4000-8000-000000000002", Amount: money.Money{Amount: 1000, Currency: "USD"}, IdempotencyKey: "create-1", OrderVersion: 1}
}
func operation(r payment.CreatePaymentIntent, ref, key string) payment.PaymentOperation {
	r.IdempotencyKey = key
	return payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: ref}
}
func code(t *testing.T, err error, want payment.ErrorCode) {
	t.Helper()
	var pe *payment.PaymentError
	if !errors.As(err, &pe) || pe.Code != want {
		t.Fatalf("error=%v want %s", err, want)
	}
}

func TestImmutableBinding(t *testing.T) {
	f := paymentfake.New()
	r := request()
	first, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*payment.CreatePaymentIntent){func(x *payment.CreatePaymentIntent) { x.Amount.Amount++ }, func(x *payment.CreatePaymentIntent) { x.Amount.Currency = "EUR" }, func(x *payment.CreatePaymentIntent) { x.OrderVersion++ }, func(x *payment.CreatePaymentIntent) { x.OrderID = "00000000-0000-4000-8000-000000000003" }} {
		changed := r
		mutate(&changed)
		_, err = f.CreateIntent(context.Background(), changed)
		code(t, err, payment.ErrorIdempotencyConflict)
	}
	replay, err := f.CreateIntent(context.Background(), r)
	if err != nil || replay != first || len(f.Events()) != 1 {
		t.Fatalf("replay=%+v err=%v events=%d", replay, err, len(f.Events()))
	}
	other := paymentfake.New()
	deterministic, err := other.CreateIntent(context.Background(), r)
	if err != nil || deterministic != first || !reflect.DeepEqual(other.Events(), f.Events()) {
		t.Fatal("fresh fake is nondeterministic")
	}
}

func TestLifecycleAndReplay(t *testing.T) {
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil || created.Status != payment.StatusAuthorized {
		t.Fatalf("create %+v %v", created, err)
	}
	capture := operation(r, created.ProviderRef, "capture-1")
	captured, err := f.Capture(context.Background(), capture)
	if err != nil || captured.Status != payment.StatusCaptured {
		t.Fatalf("capture %+v %v", captured, err)
	}
	_, err = f.Cancel(context.Background(), operation(r, created.ProviderRef, "cancel-1"))
	code(t, err, payment.ErrorStateConflict)
	refund := operation(r, created.ProviderRef, "refund-1")
	refund.Amount.Amount = 400
	partial, err := f.Refund(context.Background(), refund)
	if err != nil || partial.Status != payment.StatusPartiallyRefunded {
		t.Fatalf("refund %+v %v", partial, err)
	}
	wire, _ := json.Marshal(f.Events()[2])
	var event map[string]any
	_ = json.Unmarshal(wire, &event)
	if partial.RefundID == "" || event["RefundID"] != partial.RefundID {
		t.Fatalf("FAKE_IDENTITY_RED: result=%+v event=%s", partial, wire)
	}
	replay, err := f.Refund(context.Background(), refund)
	if err != nil || replay != partial || len(f.Events()) != 3 {
		t.Fatal("refund replay caused effects")
	}
	refund.IdempotencyKey = "refund-2"
	refund.Amount.Amount = 601
	_, err = f.Refund(context.Background(), refund)
	code(t, err, payment.ErrorAmountMismatch)
	refund.Amount.Amount = 600
	full, err := f.Refund(context.Background(), refund)
	if err != nil || full.Status != payment.StatusRefunded {
		t.Fatalf("full refund %+v %v", full, err)
	}
	for _, event := range f.Events() {
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
		replay, err := f.Replay(event.EventID)
		if err != nil || replay != event {
			t.Fatal("event replay not identical")
		}
	}
	snapshot := f.Events()
	snapshot[0].EventID = "corrupted"
	if f.Events()[0].EventID == "corrupted" {
		t.Fatal("mutable event snapshot")
	}
	_, err = f.Replay("missing")
	code(t, err, payment.ErrorNotFound)
}

func TestFailureRetryAndBinding(t *testing.T) {
	f := paymentfake.New()
	r := request()
	failure := &payment.PaymentError{Code: payment.ErrorRetryable, Message: "timeout"}
	f.FailNext(paymentfake.Create, failure)
	_, err := f.CreateIntent(context.Background(), r)
	code(t, err, payment.ErrorRetryable)
	if len(f.Events()) != 0 {
		t.Fatal("failed create emitted event")
	}
	changed := r
	changed.Amount.Amount++
	_, err = f.CreateIntent(context.Background(), changed)
	code(t, err, payment.ErrorIdempotencyConflict)
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Capture(ctx, operation(r, created.ProviderRef, "capture"))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.FailNext(paymentfake.Capture, failure)
	_, err = f.Capture(context.Background(), operation(r, created.ProviderRef, "capture"))
	code(t, err, payment.ErrorRetryable)
	if len(f.Events()) != 1 {
		t.Fatal("failed capture mutated state")
	}
	_, err = f.Capture(context.Background(), operation(r, created.ProviderRef, "capture"))
	if err != nil || len(f.Events()) != 2 {
		t.Fatal("retry failed")
	}
}

func TestConcurrentReplay(t *testing.T) {
	f := paymentfake.New()
	r := request()
	var wg sync.WaitGroup
	results := make(chan payment.PaymentIntentResult, 64)
	errs := make(chan error, 64)
	for range 64 {
		wg.Go(func() { got, err := f.CreateIntent(context.Background(), r); results <- got; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	var ref string
	for got := range results {
		if ref == "" {
			ref = got.ProviderRef
		}
		if got.ProviderRef != ref {
			t.Fatal("duplicate intent")
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Events()) != 1 {
		t.Fatal("duplicate events")
	}
	for range 64 {
		wg.Go(func() {
			_, err := f.Capture(context.Background(), operation(r, ref, "capture"))
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(f.Events()) != 2 {
		t.Fatal("duplicate capture")
	}
}

func TestTenantAndAmountAuthority(t *testing.T) {
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	op := operation(r, created.ProviderRef, "capture")
	op.TenantID = "00000000-0000-4000-8000-000000000003"
	_, err = f.Capture(context.Background(), op)
	code(t, err, payment.ErrorNotFound)
	for _, mutate := range []func(*payment.PaymentOperation){func(x *payment.PaymentOperation) { x.Amount.Amount-- }, func(x *payment.PaymentOperation) { x.Amount.Currency = "EUR" }, func(x *payment.PaymentOperation) { x.OrderVersion++ }, func(x *payment.PaymentOperation) { x.OrderID = "00000000-0000-4000-8000-000000000003" }} {
		op = operation(r, created.ProviderRef, "mismatch")
		mutate(&op)
		_, err = f.Capture(context.Background(), op)
		code(t, err, payment.ErrorAmountMismatch)
	}
	if len(f.Events()) != 1 {
		t.Fatal("invalid request produced side effect")
	}
}

func TestCancelAndInvalidRequests(t *testing.T) {
	f := paymentfake.New()
	r := request()
	for _, mutate := range []func(*payment.CreatePaymentIntent){func(x *payment.CreatePaymentIntent) { x.Amount.Amount = 0 }, func(x *payment.CreatePaymentIntent) { x.Amount.Amount = -1 }, func(x *payment.CreatePaymentIntent) { x.Amount.Currency = "usd" }, func(x *payment.CreatePaymentIntent) { x.TenantID = "bad" }, func(x *payment.CreatePaymentIntent) { x.OrderID = "" }, func(x *payment.CreatePaymentIntent) { x.IdempotencyKey = "" }, func(x *payment.CreatePaymentIntent) { x.OrderVersion = 0 }} {
		invalid := r
		mutate(&invalid)
		_, err := f.CreateIntent(context.Background(), invalid)
		code(t, err, payment.ErrorNonRetryable)
	}
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := f.Cancel(context.Background(), operation(r, created.ProviderRef, "cancel"))
	if err != nil || canceled.Status != payment.StatusCanceled {
		t.Fatal(err)
	}
	_, err = f.Capture(context.Background(), operation(r, created.ProviderRef, "capture"))
	code(t, err, payment.ErrorStateConflict)
}

func TestCaptureCancelRace(t *testing.T) {
	for range 32 {
		f := paymentfake.New()
		r := request()
		created, err := f.CreateIntent(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := f.Capture(context.Background(), operation(r, created.ProviderRef, "capture"))
			errs <- err
		}()
		go func() {
			<-start
			_, err := f.Cancel(context.Background(), operation(r, created.ProviderRef, "cancel"))
			errs <- err
		}()
		close(start)
		first, second := <-errs, <-errs
		if (first == nil) == (second == nil) {
			t.Fatalf("wanted one winner: %v %v", first, second)
		}
		if first != nil {
			code(t, first, payment.ErrorStateConflict)
		}
		if second != nil {
			code(t, second, payment.ErrorStateConflict)
		}
		events := f.Events()
		if len(events) != 2 || (events[1].Status != payment.StatusCaptured && events[1].Status != payment.StatusCanceled) {
			t.Fatal("race emitted duplicate terminal effects")
		}
	}
}

func TestRefundRace(t *testing.T) {
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Capture(context.Background(), operation(r, created.ProviderRef, "capture"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, key := range []string{"refund-1", "refund-2"} {
		go func() {
			<-start
			op := operation(r, created.ProviderRef, key)
			op.Amount.Amount = 600
			_, err := f.Refund(context.Background(), op)
			errs <- err
		}()
	}
	close(start)
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("wanted one refund: %v %v", first, second)
	}
	if first != nil {
		code(t, first, payment.ErrorAmountMismatch)
	}
	if second != nil {
		code(t, second, payment.ErrorAmountMismatch)
	}
	if len(f.Events()) != 3 {
		t.Fatal("overshot refund")
	}
}

func TestOperationNamespaceAndFailureCopy(t *testing.T) {
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Capture(context.Background(), operation(r, created.ProviderRef, r.IdempotencyKey))
	if !errors.Is(err, payment.ErrKeyReused) {
		t.Fatal("cross-operation key reuse accepted")
	}
	failure := &payment.PaymentError{Code: payment.ErrorRetryable, Message: "timeout"}
	f.FailNext(paymentfake.Cancel, failure)
	failure.Code = payment.ErrorNonRetryable
	_, err = f.Cancel(context.Background(), operation(r, created.ProviderRef, "cancel"))
	code(t, err, payment.ErrorRetryable)
	_, err = f.Cancel(context.Background(), operation(r, created.ProviderRef, "cancel"))
	if err != nil {
		t.Fatal(err)
	}
	other := r
	other.TenantID = "00000000-0000-4000-8000-000000000003"
	independent, err := f.CreateIntent(context.Background(), other)
	if err != nil || independent.ProviderRef == created.ProviderRef {
		t.Fatal("tenant namespace collision")
	}
}

func TestObservedCaptureRetainsBindingAndRefundBalance(t *testing.T) {
	ctx := context.Background()
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	event := payment.VerifiedEvent{EventID: "committed-capture", TenantID: r.TenantID, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: created.ProviderRef, Status: payment.StatusCaptured, Amount: r.Amount}
	external := event
	external.ProviderRef = "fake_external_capture"
	if err = f.ObserveCaptured(external); err != nil {
		t.Fatal("external capture fact was rejected", err)
	}
	_, err = f.Refund(ctx, operation(r, external.ProviderRef, "external-refund"))
	code(t, err, payment.ErrorNotFound)
	for _, mutate := range []func(*payment.VerifiedEvent){func(e *payment.VerifiedEvent) { e.TenantID = "00000000-0000-4000-8000-000000000003" }, func(e *payment.VerifiedEvent) { e.OrderID = "00000000-0000-4000-8000-000000000003" }, func(e *payment.VerifiedEvent) { e.OrderVersion++ }, func(e *payment.VerifiedEvent) { e.Amount.Amount++ }, func(e *payment.VerifiedEvent) { e.Amount.Currency = "EUR" }} {
		bad := event
		mutate(&bad)
		code(t, f.ObserveCaptured(bad), payment.ErrorAmountMismatch)
	}
	refund := operation(r, created.ProviderRef, "partial")
	refund.Amount.Amount = 400
	_, err = f.Refund(ctx, refund)
	code(t, err, payment.ErrorStateConflict)
	if err = f.ObserveCaptured(event); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Refund(ctx, refund); err != nil {
		t.Fatal(err)
	}
	if err = f.ObserveCaptured(event); err != nil {
		t.Fatal(err)
	}
	rest := operation(r, created.ProviderRef, "rest")
	rest.Amount.Amount = 600
	result, err := f.Refund(ctx, rest)
	if err != nil || result.Status != payment.StatusRefunded {
		t.Fatalf("full refund: %+v %v", result, err)
	}
	if err = f.ObserveCaptured(event); err != nil {
		t.Fatal(err)
	}
	_, err = f.Refund(ctx, operation(r, created.ProviderRef, "excess"))
	code(t, err, payment.ErrorStateConflict)
	if len(f.Events()) != 3 {
		t.Fatal("capture observation issued a provider command")
	}
}
