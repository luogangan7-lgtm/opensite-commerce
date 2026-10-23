//go:build pgx

// Responsibility: prove real-provider admission uses persisted cart consent.
// Dependencies: existing migrated checkout fixture; caller: CI pgx lane.
// Invariant: rejected sources/money/keys never authorize any provider request.
package db_test

import (
	"context"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
)

func TestStripePGConsentAdmission(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 10)
	start := f.start(t)
	admission, ok := f.service.(interface {
		AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error
	})
	if !ok {
		t.Fatal("real-provider admission missing: AdoptOrder/legacy sources must fail closed")
	}
	r := payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: start.OrderID, OrderVersion: start.OrderVersion, Amount: money.Money{Amount: start.AmountMinor, Currency: start.Currency}, IdempotencyKey: "checkout:v1:" + start.CheckoutID + ":create"}
	// A ready session must not mint a fresh intent or bypass the capture claim.
	if admission.AdmitRealPayment(f.ctx, r, false) == nil || admission.AdmitRealPayment(f.ctx, r, true) == nil {
		t.Fatal("phase/key bypass admitted")
	}

	a := newCheckoutAdoptionPG(t)
	adopted, err := a.adopter.AdoptOrder(a.f.ctx, a.f.actor, a.order.ID, "stripe-no-adoption")
	if err != nil {
		t.Fatal(err)
	}
	check := a.f.service.(interface {
		AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error
	})
	if check.AdmitRealPayment(a.f.ctx, payment.CreatePaymentIntent{TenantID: a.f.tenant, OrderID: a.order.ID, OrderVersion: adopted.OrderVersion, Amount: money.Money{Amount: adopted.AmountMinor, Currency: adopted.Currency}, IdempotencyKey: "checkout:v1:" + adopted.CheckoutID + ":create"}, false) == nil {
		t.Fatal("AdoptOrder admitted without cart consent")
	}
	r.IdempotencyKey = "checkout:v1:" + start.CheckoutID + ":capture"
	f.provider.before = func(ctx context.Context, op paymentfake.Operation) error {
		if op == paymentfake.Capture {
			for _, change := range []func(*payment.CreatePaymentIntent){
				func(r *payment.CreatePaymentIntent) { r.Amount.Amount-- },
				func(r *payment.CreatePaymentIntent) { r.Amount.Currency = "EUR" },
				func(r *payment.CreatePaymentIntent) { r.OrderVersion++ },
				func(r *payment.CreatePaymentIntent) { r.IdempotencyKey = "forged" },
			} {
				forged := r
				change(&forged)
				if admission.AdmitRealPayment(ctx, forged, true) == nil {
					t.Fatal("forged payable admitted")
				}
			}
			return admission.AdmitRealPayment(ctx, r, true)
		}
		return nil
	}
	if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), f.event(start, payment.StatusAuthorized)); err != nil {
		t.Fatalf("exact durable consent rejected: %v", err)
	}
}
