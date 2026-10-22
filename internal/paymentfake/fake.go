// Package paymentfake is test-only deterministic Payment Port wiring for Checkout
// gates. It depends on payment/v1 and stdlib, owns no tables or provider secrets,
// and must never be registered as a production payment provider. Its events are
// test fixtures, not evidence of webhook signature/account verification.
package paymentfake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

type Operation string

const (
	Create  Operation = "create"
	Capture Operation = "capture"
	Cancel  Operation = "cancel"
	Refund  Operation = "refund"
)

// Event deliberately differs from VerifiedEvent: the fake performs no production
// verification. Test consumers can inspect/replay its stable ID and fields.
type Event payment.VerifiedEvent

func (e Event) Validate() error { return payment.VerifiedEvent(e).Validate() }

type binding struct {
	fingerprint string
	result      payment.PaymentIntentResult
	failure     *payment.PaymentError
	complete    bool
}
type intent struct {
	request  payment.CreatePaymentIntent
	status   payment.PaymentStatus
	refunded int64
}

type Fake struct {
	// ponytail: one lock serializes test operations; shard only if test throughput requires it.
	mu       sync.Mutex
	bindings map[string]binding
	intents  map[string]intent
	failures map[Operation][]*payment.PaymentError
	events   []Event
}

var _ payment.Port = (*Fake)(nil)

func New() *Fake {
	return &Fake{bindings: make(map[string]binding), intents: make(map[string]intent), failures: make(map[Operation][]*payment.PaymentError)}
}

// FailNext injects one boundary error before side effects. The accepted request
// still binds its key. Retryable errors may resume; a non-retryable rejection
// is permanent for that key and always replays without creating effects.
// Copying the error keeps caller mutation from changing a scheduled failure.
func (f *Fake) FailNext(op Operation, err *payment.PaymentError) {
	if err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := *err
	f.failures[op] = append(f.failures[op], &copied)
}
func (f *Fake) CreateIntent(ctx context.Context, r payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	return f.apply(ctx, Create, payment.PaymentOperation{CreatePaymentIntent: r})
}
func (f *Fake) Capture(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return f.apply(ctx, Capture, r)
}
func (f *Fake) Cancel(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return f.apply(ctx, Cancel, r)
}
func (f *Fake) Refund(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return f.apply(ctx, Refund, r)
}

func digest(value any) string {
	raw, _ := json.Marshal(value) // only fixed string/integer structs; marshaling cannot fail.
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func failure(code payment.ErrorCode, message string) error {
	return &payment.PaymentError{Code: code, Message: message}
}

func (f *Fake) apply(ctx context.Context, op Operation, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	var zero payment.PaymentIntentResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := r.CreatePaymentIntent.Validate(); err != nil {
		return zero, err
	}
	if op != Create {
		if err := r.Validate(); err != nil {
			return zero, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	key := r.TenantID + "/" + r.IdempotencyKey
	fingerprint := digest(struct {
		Operation Operation
		Request   payment.PaymentOperation
	}{op, r})
	bound, exists := f.bindings[key]
	if exists && bound.fingerprint != fingerprint {
		return zero, payment.ErrKeyReused
	}
	if exists && bound.complete {
		if bound.failure != nil {
			copied := *bound.failure
			return zero, &copied
		}
		return bound.result, nil
	}
	ref := r.ProviderRef
	var current intent
	if op == Create {
		ref = "fake_intent_" + fingerprint
		current = intent{request: r.CreatePaymentIntent, status: payment.StatusAuthorized}
	} else {
		var found bool
		current, found = f.intents[ref]
		if !found || current.request.TenantID != r.TenantID {
			return zero, failure(payment.ErrorNotFound, "payment not found")
		}
		if current.request.OrderID != r.OrderID || current.request.OrderVersion != r.OrderVersion || current.request.Amount.Currency != r.Amount.Currency || (op != Refund && current.request.Amount.Amount != r.Amount.Amount) {
			return zero, failure(payment.ErrorAmountMismatch, "payable snapshot mismatch")
		}
		switch op {
		case Capture:
			if current.status != payment.StatusAuthorized {
				return zero, failure(payment.ErrorStateConflict, "capture requires authorization")
			}
			current.status = payment.StatusCaptured
		case Cancel:
			if current.status != payment.StatusAuthorized {
				return zero, failure(payment.ErrorStateConflict, "cancel requires authorization")
			}
			current.status = payment.StatusCanceled
		case Refund:
			if current.status != payment.StatusCaptured && current.status != payment.StatusPartiallyRefunded {
				return zero, failure(payment.ErrorStateConflict, "refund requires captured payment")
			}
			if r.Amount.Amount > current.request.Amount.Amount-current.refunded {
				return zero, failure(payment.ErrorAmountMismatch, "refund exceeds captured balance")
			}
			current.refunded += r.Amount.Amount
			current.status = payment.StatusPartiallyRefunded
			if current.refunded == current.request.Amount.Amount {
				current.status = payment.StatusRefunded
			}
		}
	}
	f.bindings[key] = binding{fingerprint: fingerprint}
	if queue := f.failures[op]; len(queue) > 0 {
		f.failures[op] = queue[1:]
		copied := *queue[0]
		copied.NoSideEffect = copied.Code == payment.ErrorNonRetryable
		if copied.Code == payment.ErrorNonRetryable {
			cached := copied
			f.bindings[key] = binding{fingerprint: fingerprint, failure: &cached, complete: true}
		}
		return zero, &copied
	}
	result := payment.PaymentIntentResult{ProviderRef: ref, Status: current.status}
	f.intents[ref] = current
	f.bindings[key] = binding{fingerprint: fingerprint, result: result, complete: true}
	f.events = append(f.events, Event{EventID: "fake_event_" + fingerprint, TenantID: r.TenantID, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: ref, Status: current.status, Amount: r.Amount})
	return result, nil
}

// Events returns an immutable snapshot in committed operation order.
func (f *Fake) Events() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.events...)
}

// Replay returns the original event without adding effects or changing its ID.
func (f *Fake) Replay(eventID string) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if event.EventID == eventID {
			return event, nil
		}
	}
	return Event{}, failure(payment.ErrorNotFound, "event not found")
}
