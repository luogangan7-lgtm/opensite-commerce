// Contract tests validate public trust-boundary shape and typed error semantics.
package payment_test

import (
	"errors"
	"strings"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestRequestAndVerifiedEventShape(t *testing.T) {
	r := payment.CreatePaymentIntent{TenantID: "00000000-0000-4000-8000-000000000001", OrderID: "00000000-0000-4000-8000-000000000002", OrderVersion: 1, Amount: money.Money{Amount: 1, Currency: "USD"}, IdempotencyKey: "key"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*payment.CreatePaymentIntent){func(x *payment.CreatePaymentIntent) { x.TenantID = "00000000-0000-0000-0000-000000000000" }, func(x *payment.CreatePaymentIntent) { x.OrderID = "00000000000040008000000000000002" }, func(x *payment.CreatePaymentIntent) { x.OrderID = "00000000-0000-4000-8000-00000000000A" }, func(x *payment.CreatePaymentIntent) { x.IdempotencyKey = "\nkey" }, func(x *payment.CreatePaymentIntent) { x.IdempotencyKey = "   " }, func(x *payment.CreatePaymentIntent) { x.IdempotencyKey = string([]byte{0xff}) }} {
		invalid := r
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("invalid request accepted")
		}
	}
	e := payment.VerifiedEvent{EventID: "event", TenantID: r.TenantID, OrderID: r.OrderID, OrderVersion: r.OrderVersion, ProviderRef: "neutral-ref", Status: payment.StatusCaptured, Amount: r.Amount}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	// EventID becomes the Orders transition EventKey: 1-128 printable ASCII.
	for _, id := range []string{"has space", "ünicode", string([]byte{0xfe}), strings.Repeat("e", 129)} {
		bad := e
		bad.EventID = id
		if bad.Validate() == nil {
			t.Fatalf("event id %q accepted but Orders would reject it", id)
		}
	}
	long := e
	long.EventID = strings.Repeat("e", 128)
	if err := long.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Status = "future_unknown"
	if e.Validate() == nil {
		t.Fatal("unknown status accepted")
	}
	e.Status = payment.StatusCaptured
	e.ProviderRef = ""
	if e.Validate() == nil {
		t.Fatal("missing reference accepted")
	}
}

func TestTypedErrorSemantics(t *testing.T) {
	e := &payment.PaymentError{Code: payment.ErrorIdempotencyConflict, Message: "provider-specific detail removed"}
	if !errors.Is(e, payment.ErrKeyReused) {
		t.Fatal("stable key reuse sentinel lost")
	}
	if e.Retryable() {
		t.Fatal("key reuse is not retryable")
	}
	if !(&payment.PaymentError{Code: payment.ErrorRetryable}).Retryable() {
		t.Fatal("retryable error not identified")
	}
}
