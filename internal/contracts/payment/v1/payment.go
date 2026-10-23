// Package payment defines the provider-neutral v1 Payment Port for Checkout and
// Orders. It depends on money and existing UUID validation; owns no tables.
// Invariants: integer money, tenant/order/version binding and stable idempotency;
// shape validation cannot establish webhook authenticity or authorize payment.
package payment

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

type PaymentStatus string

const (
	StatusPending           PaymentStatus = "pending"
	StatusRequiresAction    PaymentStatus = "requires_action"
	StatusAuthorized        PaymentStatus = "authorized"
	StatusCaptured          PaymentStatus = "captured"
	StatusFailed            PaymentStatus = "failed"
	StatusCanceled          PaymentStatus = "canceled"
	StatusPartiallyRefunded PaymentStatus = "partially_refunded"
	StatusRefunded          PaymentStatus = "refunded"
)

// CreatePaymentIntent uses the authoritative GetPayable snapshot. Amount includes
// currency; OrderVersion identifies that snapshot, not a provider revision.
// An idempotency key is scoped to the tenant and bound to operation + full input.
type CreatePaymentIntent struct {
	TenantID       string
	OrderID        string
	Amount         money.Money
	IdempotencyKey string
	OrderVersion   int64
}

type PaymentOperation struct {
	CreatePaymentIntent
	ProviderRef string
}

type NextAction struct {
	Kind string
	URL  string
}

type PaymentIntentResult struct {
	ProviderRef string
	Status      PaymentStatus
	NextAction  NextAction
}

// C-19b real-provider consent preconditions (including existing-order adoption)
// are defined in docs/architecture/V1_DECISIONS.md, section "C-19b consent contract".
// Port requests do not prove payment to Orders. A successful client-side return
// must never substitute for a trusted VerifiedEvent.
type Port interface {
	CreateIntent(context.Context, CreatePaymentIntent) (PaymentIntentResult, error)
	Capture(context.Context, PaymentOperation) (PaymentIntentResult, error)
	Cancel(context.Context, PaymentOperation) (PaymentIntentResult, error)
	Refund(context.Context, PaymentOperation) (PaymentIntentResult, error)
}

type ErrorCode string

const (
	ErrorRetryable           ErrorCode = "retryable"
	ErrorNonRetryable        ErrorCode = "non_retryable"
	ErrorAmountMismatch      ErrorCode = "amount_mismatch"
	ErrorStateConflict       ErrorCode = "state_conflict"
	ErrorIdempotencyConflict ErrorCode = "idempotency_conflict"
	ErrorNotFound            ErrorCode = "not_found"
)

type PaymentError struct {
	Code    ErrorCode
	Message string

	// NoSideEffect is true only when the trusted adapter establishes that this
	// exact operation/key is permanently sealed without side effects: no prior
	// effect and no concurrent or future replay can succeed. The default false
	// means unknown; a non-retryable code alone does not prove absence.
	NoSideEffect bool
}

func (e *PaymentError) Error() string   { return string(e.Code) + ": " + e.Message }
func (e *PaymentError) Retryable() bool { return e.Code == ErrorRetryable }
func (e *PaymentError) Is(target error) bool {
	other, ok := target.(*PaymentError)
	return ok && e.Code == other.Code
}

var ErrKeyReused = &PaymentError{Code: ErrorIdempotencyConflict, Message: "idempotency key already bound to different request"}

func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (r CreatePaymentIntent) Validate() error {
	if !validID(r.TenantID) || !validID(r.OrderID) || r.OrderVersion <= 0 || r.Amount.Amount <= 0 || r.Amount.Validate() != nil || strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 200 || strings.ContainsAny(r.IdempotencyKey, "\r\n\x00") || !utf8.ValidString(r.IdempotencyKey) {
		return &PaymentError{Code: ErrorNonRetryable, Message: "invalid payment request"}
	}
	return nil
}
func (r PaymentOperation) Validate() error {
	if err := r.CreatePaymentIntent.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.ProviderRef) == "" {
		return &PaymentError{Code: ErrorNonRetryable, Message: "provider reference required"}
	}
	return nil
}

// VerifiedEvent is the trusted service-to-Orders contract. Only the Payments
// service may submit it AFTER raw webhook signature verification, trusted account
// to tenant mapping and durable Inbox admission. EventID is a stable delivery
// deduplication identifier; replay preserves all fields. Validate checks only
// shape and does NOT perform these trust checks. No public webhook may decode
// directly into this type and mark an order paid.
// Amount is the amount of this event (refund delta for refund events).
type VerifiedEvent struct {
	EventID      string
	TenantID     string
	OrderID      string
	OrderVersion int64
	ProviderRef  string
	Status       PaymentStatus
	Amount       money.Money
}

func (e VerifiedEvent) Validate() error {
	switch e.Status {
	case StatusPending, StatusRequiresAction, StatusAuthorized, StatusCaptured, StatusFailed, StatusCanceled, StatusPartiallyRefunded, StatusRefunded:
	default:
		return &PaymentError{Code: ErrorNonRetryable, Message: "invalid payment event status"}
	}
	// EventID is submitted as the Orders transition EventKey: 1-128 printable ASCII.
	if len(e.EventID) < 1 || len(e.EventID) > 128 || strings.ContainsFunc(e.EventID, func(c rune) bool { return c < '!' || c > '~' }) {
		return &PaymentError{Code: ErrorNonRetryable, Message: "invalid payment event id"}
	}
	return (PaymentOperation{CreatePaymentIntent: CreatePaymentIntent{TenantID: e.TenantID, OrderID: e.OrderID, OrderVersion: e.OrderVersion, Amount: e.Amount, IdempotencyKey: e.EventID}, ProviderRef: e.ProviderRef}).Validate()
}
