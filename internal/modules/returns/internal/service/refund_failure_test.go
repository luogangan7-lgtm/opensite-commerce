// Responsibilities: provider rejection releases only known-no-effect reservations.
// Dependencies: service fixture and neutral Payment Port; callers: CI Go gate.
// Invariants: uncertain outcomes remain pending and failed intents cannot complete.
package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

type rejectedRefundPort struct {
	payment.Port
	err error
}

func (p rejectedRefundPort) Refund(context.Context, payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, p.err
}

type failureRefundRepo struct {
	*refundRepo
	failures int
	failErr  error
}

func (r *failureRefundRepo) Fail(_ context.Context, _ pgx.Tx, _ string, in contract.PreparedRefund, code string) error {
	if r.failErr != nil {
		return r.failErr
	}
	if r.prepared.RefundKey != in.RefundKey || code == "" {
		return contract.ErrKeyReused
	}
	r.failures++
	r.prepared.Status = "refund_failed"
	return nil
}

func TestRefundProviderFailureState(t *testing.T) {
	for _, code := range []payment.ErrorCode{payment.ErrorAmountMismatch, payment.ErrorStateConflict, payment.ErrorIdempotencyConflict, payment.ErrorNonRetryable, payment.ErrorNotFound} {
		t.Run(string(code), func(t *testing.T) {
			s, original, p, _, _, l, runner := refundFixture(t)
			repo := &failureRefundRepo{refundRepo: original}
			s.repo = repo
			rejection := &payment.PaymentError{Code: code, Message: "fixture rejection"}
			s.config.Payments = rejectedRefundPort{Port: p, err: fmt.Errorf("wrapped: %w", rejection)}
			in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "definitive-failure"}
			if _, err := s.Refund(context.Background(), tenantID, in); !errors.Is(err, rejection) {
				t.Fatalf("provider rejection lost: %v", err)
			}
			if repo.failures != 1 || repo.prepared.Status != "refund_failed" || runner.commits != 2 || l.calls != 0 || repo.completed {
				t.Fatalf("definitive rejection stranded pending intent: failures=%d status=%s commits=%d journal=%d", repo.failures, repo.prepared.Status, runner.commits, l.calls)
			}
			if _, err := s.Refund(context.Background(), tenantID, in); !errors.Is(err, contract.ErrConflict) || repo.failures != 1 {
				t.Fatalf("failed intent replay contacted provider or revived: %v failures=%d", err, repo.failures)
			}
		})
	}
}

func TestRefundUncertainFailureRetainsReservation(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, errors.New("unknown transport outcome"), &payment.PaymentError{Code: payment.ErrorRetryable, Message: "retry later"}} {
		s, r, p, _, _, l, runner := refundFixture(t)
		repo := &failureRefundRepo{refundRepo: r}
		s.repo = repo
		s.config.Payments = rejectedRefundPort{Port: p, err: err}
		_, got := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "uncertain"})
		if !errors.Is(got, err) || repo.failures != 0 || repo.prepared.Status != "refund_pending" || runner.commits != 1 || l.calls != 0 {
			t.Fatalf("uncertain refund released: %v %+v commits=%d", got, repo, runner.commits)
		}
	}
}

func TestRefundFailurePersistenceErrorSurfaces(t *testing.T) {
	s, r, p, _, _, _, _ := refundFixture(t)
	repo := &failureRefundRepo{refundRepo: r, failErr: contract.ErrRetry}
	s.repo = repo
	s.config.Payments = rejectedRefundPort{Port: p, err: &payment.PaymentError{Code: payment.ErrorAmountMismatch}}
	_, err := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "persistence-error"})
	if !errors.Is(err, contract.ErrRetry) || repo.prepared.Status != "refund_pending" {
		t.Fatalf("failure persistence was reported as successful: %v status=%s", err, repo.prepared.Status)
	}
}
