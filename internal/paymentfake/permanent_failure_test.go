// Responsibility: prove definitive Fake failures cannot become successful on
// concurrent replay. Dependencies: Payment Port/Fake; checkout's safety gates
// call these tests. Permanent results bind the full operation and input forever.
package paymentfake_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
)

func TestPermanentFailureReplayHasNoEffects(t *testing.T) {
	for _, op := range []paymentfake.Operation{paymentfake.Create, paymentfake.Capture} {
		t.Run(string(op), func(t *testing.T) {
			f := paymentfake.New()
			r := request()
			var capture payment.PaymentOperation
			before := 0
			if op == paymentfake.Capture {
				intent, err := f.CreateIntent(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				capture = operation(r, intent.ProviderRef, "permanent-capture")
				before = 1
			}
			f.FailNext(op, &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "definitive rejection"})
			call := func() error {
				if op == paymentfake.Capture {
					_, err := f.Capture(context.Background(), capture)
					return err
				}
				_, err := f.CreateIntent(context.Background(), r)
				return err
			}
			first := call()
			code(t, first, payment.ErrorNonRetryable)
			var copied *payment.PaymentError
			if !errors.As(first, &copied) {
				t.Fatal(first)
			}
			copied.Code = payment.ErrorRetryable // A returned error is caller-owned.
			var wg sync.WaitGroup
			errs := make(chan error, 32)
			for range 32 {
				wg.Go(func() { errs <- call() })
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				code(t, err, payment.ErrorNonRetryable)
			}
			if len(f.Events()) != before {
				t.Fatal("permanent rejection replay created provider effects")
			}
			r.Amount.Amount++
			if op == paymentfake.Create {
				_, err := f.CreateIntent(context.Background(), r)
				code(t, err, payment.ErrorIdempotencyConflict)
			} else {
				capture.Amount.Amount++
				_, err := f.Capture(context.Background(), capture)
				code(t, err, payment.ErrorIdempotencyConflict)
				capture.Amount.Amount--
				capture.IdempotencyKey = "cancel-after-permanent-failure"
				_, err = f.Cancel(context.Background(), capture)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRefundNoSideEffectEvidenceAndSuccessfulReplay(t *testing.T) {
	f := paymentfake.New()
	r := request()
	created, err := f.CreateIntent(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Capture(context.Background(), operation(r, created.ProviderRef, "capture")); err != nil {
		t.Fatal(err)
	}
	refund := operation(r, created.ProviderRef, "rejected-refund")
	refund.Amount.Amount = 400
	rejection := &payment.PaymentError{Code: payment.ErrorNonRetryable, Message: "definitive rejection"}
	if rejection.NoSideEffect {
		t.Fatal("non-retryable code alone asserted no side effect")
	}
	f.FailNext(paymentfake.Refund, rejection)
	for attempt := 0; attempt < 2; attempt++ {
		_, err = f.Refund(context.Background(), refund)
		var provider *payment.PaymentError
		if !errors.As(err, &provider) || provider.Code != payment.ErrorNonRetryable || !provider.NoSideEffect {
			t.Fatalf("attempt=%d rejected refund lacks no-side-effect evidence: %v", attempt, err)
		}
		provider.NoSideEffect = false // Caller mutation cannot weaken the cached evidence.
	}
	if len(f.Events()) != 2 {
		t.Fatal("rejected refund or replay caused a provider effect")
	}
	changed := refund
	changed.Amount.Amount++
	_, err = f.Refund(context.Background(), changed)
	var provider *payment.PaymentError
	if !errors.As(err, &provider) || provider.Code != payment.ErrorIdempotencyConflict || provider.NoSideEffect {
		t.Fatalf("key conflict asserted no side effect: %v", err)
	}
	refund.IdempotencyKey = "successful-refund"
	first, err := f.Refund(context.Background(), refund)
	if err != nil || first.Status != payment.StatusPartiallyRefunded {
		t.Fatalf("refund result=%+v err=%v", first, err)
	}
	changed = refund
	changed.Amount.Amount++
	_, err = f.Refund(context.Background(), changed)
	if !errors.As(err, &provider) || provider.Code != payment.ErrorIdempotencyConflict || provider.NoSideEffect {
		t.Fatalf("key conflict after provider effect asserted no side effect: %v", err)
	}
	replay, err := f.Refund(context.Background(), refund)
	if err != nil || replay != first || len(f.Events()) != 3 {
		t.Fatalf("successful refund replay changed result/effects: result=%+v err=%v events=%d", replay, err, len(f.Events()))
	}
}
