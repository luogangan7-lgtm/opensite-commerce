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
