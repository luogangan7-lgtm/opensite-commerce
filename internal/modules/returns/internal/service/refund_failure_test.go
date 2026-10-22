// Responsibilities: prove durable provider-failure classification and recovery.
// Dependencies: shared refund fixtures and neutral Payment Port; caller: CI Go gate.
// Invariants: only trusted no-effect proof releases a reservation; uncertain
// operations keep their frozen fingerprint even after caller cancellation.
package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

type rejectedRefundPort struct {
	payment.Port
	err         error
	requests    []payment.PaymentOperation
	applyEffect bool
	cancel      context.CancelFunc
}

func (p *rejectedRefundPort) Refund(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	p.requests = append(p.requests, in)
	if p.applyEffect {
		if _, err := p.Port.Refund(ctx, in); err != nil {
			return payment.PaymentIntentResult{}, err
		}
	}
	if p.cancel != nil {
		p.cancel()
	}
	return payment.PaymentIntentResult{}, p.err
}

// Keep the shared success fixture compatible with the durable-failure port.
func (r *refundRepo) Fail(_ context.Context, _ pgx.Tx, _ string, in contract.PreparedRefund, rejected bool) error {
	if !r.pending || !sameIntent(r.prepared, in) {
		return contract.ErrKeyReused
	}
	r.prepared.Status = "refund_reconcile"
	if rejected {
		r.prepared.Status = "refund_failed"
	}
	return nil
}

type failureRefundRepo struct {
	*refundRepo
	failures int
	failErr  error
	observe  func(context.Context, bool)
}

func (r *failureRefundRepo) Fail(ctx context.Context, tx pgx.Tx, tenant string, in contract.PreparedRefund, rejected bool) error {
	r.failures++
	if r.observe != nil {
		r.observe(ctx, rejected)
	}
	if r.failErr != nil {
		return r.failErr
	}
	return r.refundRepo.Fail(ctx, tx, tenant, in, rejected)
}

func TestRefundProviderFailureState(t *testing.T) {
	for _, code := range []payment.ErrorCode{payment.ErrorAmountMismatch, payment.ErrorStateConflict, payment.ErrorIdempotencyConflict, payment.ErrorNonRetryable, payment.ErrorNotFound} {
		t.Run(string(code), func(t *testing.T) {
			s, original, p, _, _, l, runner := refundFixture(t)
			repo := &failureRefundRepo{refundRepo: original}
			s.repo = repo
			rejection := &payment.PaymentError{Code: code, Message: "fixture rejection", NoSideEffect: true}
			provider := &rejectedRefundPort{Port: p, err: fmt.Errorf("wrapped: %w", rejection)}
			s.config.Payments = provider
			repo.observe = func(_ context.Context, rejected bool) {
				if !runner.active || runner.commits != 1 || !rejected {
					t.Error("rejection must persist in a second transaction")
				}
			}
			in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "definitive-failure"}
			if _, err := s.Refund(context.Background(), tenantID, in); !errors.Is(err, rejection) {
				t.Fatalf("provider rejection lost: %v", err)
			}
			if repo.failures != 1 || repo.prepared.Status != "refund_failed" || runner.commits != 2 || l.calls != 0 || repo.completed || len(p.Events()) != 2 {
				t.Fatalf("rejection state: failures=%d status=%s commits=%d journal=%d", repo.failures, repo.prepared.Status, runner.commits, l.calls)
			}
			if _, err := s.Refund(context.Background(), tenantID, in); !errors.Is(err, contract.ErrConflict) || repo.failures != 1 || len(provider.requests) != 1 || l.calls != 0 {
				t.Fatalf("failed replay revived intent: err=%v failures=%d requests=%d", err, repo.failures, len(provider.requests))
			}
		})
	}
}

func TestRefundUncertainFailureRetainsReservation(t *testing.T) {
	cases := map[string]error{
		"timeout":           context.DeadlineExceeded,
		"unknown transport": errors.New("unknown transport outcome"),
		"unknown code":      &payment.PaymentError{Code: "unknown"},
		"default error":     &payment.PaymentError{},
		"retryable":         &payment.PaymentError{Code: payment.ErrorRetryable},
		"marked retryable":  &payment.PaymentError{Code: payment.ErrorRetryable, NoSideEffect: true},
	}
	for _, code := range []payment.ErrorCode{payment.ErrorAmountMismatch, payment.ErrorStateConflict, payment.ErrorIdempotencyConflict, payment.ErrorNonRetryable, payment.ErrorNotFound} {
		cases[string(code)] = &payment.PaymentError{Code: code}
	}
	for name, cause := range cases {
		t.Run(name, func(t *testing.T) {
			s, r, p, captures, accounts, l, runner := refundFixture(t)
			repo := &failureRefundRepo{refundRepo: r}
			s.repo = repo
			// Model provider success followed by a lost response, including a
			// misleading rejection code without trusted no-effect evidence.
			provider := &rejectedRefundPort{Port: p, err: cause, applyEffect: true}
			s.config.Payments = provider
			repo.observe = func(_ context.Context, rejected bool) {
				if rejected || !runner.active || runner.commits != 1 {
					t.Error("uncertain outcome cannot release its reservation")
				}
			}
			in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "uncertain"}
			_, got := s.Refund(context.Background(), tenantID, in)
			if !errors.Is(got, cause) || repo.failures != 1 || repo.prepared.Status != "refund_reconcile" || !repo.pending || runner.commits != 2 || l.calls != 0 || repo.completes != 0 {
				t.Fatalf("uncertain reservation lost: err=%v status=%s failures=%d commits=%d", got, repo.prepared.Status, repo.failures, runner.commits)
			}
			frozen := repo.prepared
			if frozen.RefundMinor != 135 || frozen.RefundKey != in.IdempotencyKey || len(provider.requests) != 1 || len(p.Events()) != 3 {
				t.Fatal("uncertain refund lost amount/key or provider effect")
			}
			captures.fail, accounts.fail = true, true
			s.config.Payments = p
			out, err := s.Refund(context.Background(), tenantID, in)
			if err != nil || out.Status != "refunded" || repo.prepares != 1 || repo.failures != 1 || l.calls != 1 || repo.completes != 1 || len(p.Events()) != 3 || !sameIntent(frozen, repo.prepared) {
				t.Fatalf("same-key recovery changed effects/fingerprint: out=%+v err=%v", out, err)
			}
			if persisted, ok := p.requests[provider.requests[0].IdempotencyKey]; !ok || persisted != provider.requests[0] || captures.calls != 1 || accounts.calls != 1 {
				t.Fatal("recovery did not reuse original PSP fingerprint")
			}
		})
	}
}

func TestRefundFailurePersistenceErrorSurfaces(t *testing.T) {
	for _, proof := range []bool{false, true} {
		s, r, p, _, _, l, runner := refundFixture(t)
		repo := &failureRefundRepo{refundRepo: r, failErr: contract.ErrRetry}
		s.repo = repo
		s.config.Payments = &rejectedRefundPort{Port: p, err: &payment.PaymentError{Code: payment.ErrorAmountMismatch, NoSideEffect: proof}}
		_, err := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "persistence-error"})
		if !errors.Is(err, contract.ErrRetry) || repo.prepared.Status != "refund_pending" || !repo.pending || repo.failures != 1 || runner.commits != 1 || l.calls != 0 {
			t.Fatalf("persistence failure hidden: err=%v status=%s commits=%d", err, repo.prepared.Status, runner.commits)
		}
	}
}

func TestRefundCanceledCallerStillPersistsFailure(t *testing.T) {
	s, r, p, _, _, l, runner := refundFixture(t)
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "retained"))
	defer cancel()
	repo := &failureRefundRepo{refundRepo: r}
	s.repo = repo
	s.config.Payments = &rejectedRefundPort{Port: p, err: context.Canceled, cancel: cancel}
	repo.observe = func(persistCtx context.Context, rejected bool) {
		deadline, bounded := persistCtx.Deadline()
		remaining := time.Until(deadline)
		if ctx.Err() != context.Canceled || persistCtx.Err() != nil || !bounded || remaining <= 0 || remaining > 5*time.Second || rejected || persistCtx.Value(contextKey{}) != "retained" || !runner.active || runner.commits != 1 {
			t.Errorf("failure persistence lacks independent bounded context: caller=%v persist=%v bounded=%v remaining=%v", ctx.Err(), persistCtx.Err(), bounded, remaining)
		}
	}
	_, err := s.Refund(ctx, tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "canceled-caller"})
	if !errors.Is(err, context.Canceled) || repo.failures != 1 || repo.prepared.Status != "refund_reconcile" || runner.commits != 2 || l.calls != 0 {
		t.Fatalf("caller cancellation skipped persistence: err=%v status=%s commits=%d", err, repo.prepared.Status, runner.commits)
	}
}
