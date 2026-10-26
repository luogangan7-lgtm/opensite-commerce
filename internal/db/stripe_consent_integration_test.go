//go:build pgx

// Responsibility: prove real-provider admission uses persisted cart consent.
// Dependencies: existing migrated checkout fixture; caller: CI pgx lane.
// Invariant: rejected sources/money/keys never authorize any provider request.
package db_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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

func TestStripePGConsentSourceGuardAtAcceptedCapture(t *testing.T) {
	// The cart control must reach and pass the very same phase/key admission.
	f := newCheckoutPGFixture(t, 10, 10)
	cart := f.start(t)
	admit := f.service.(interface { AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error })
	cartCalls := 0
	f.provider.before = func(ctx context.Context, op paymentfake.Operation) error {
		if op != paymentfake.Capture { return nil }
		cartCalls++
		return admit.AdmitRealPayment(ctx, payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: cart.OrderID, OrderVersion: cart.OrderVersion, Amount: money.Money{Amount: cart.AmountMinor, Currency: cart.Currency}, IdempotencyKey: "checkout:v1:"+cart.CheckoutID+":capture"}, true)
	}
	if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), f.event(cart, payment.StatusAuthorized)); err != nil || cartCalls != 1 { t.Fatalf("cart capture control not accepted: calls=%d err=%v", cartCalls, err) }

	a := newCheckoutAdoptionPG(t)
	adopted := a.adopt(t, "stripe-source-guard")
	check := a.f.service.(interface { AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error })
	denied := errors.New("adopted source deliberately blocked before provider IO")
	adoptCalls := 0
	a.f.provider.before = func(ctx context.Context, op paymentfake.Operation) error {
		if op != paymentfake.Capture { return nil }
		adoptCalls++
		request := payment.CreatePaymentIntent{TenantID: a.f.tenant, OrderID: adopted.OrderID, OrderVersion: adopted.OrderVersion, Amount: money.Money{Amount: adopted.AmountMinor, Currency: adopted.Currency}, IdempotencyKey: "checkout:v1:"+adopted.CheckoutID+":capture"}
		if check.AdmitRealPayment(ctx, request, true) == nil { t.Error("ADOPT_SOURCE_RED: existing order without consent admitted") }
		// A deliberately corrupted receipt supplies every other accepted field.
		// Only this disposable maintenance transaction bypasses immutable triggers;
		// source, phase, actor, payable, key and provider command remain unchanged.
		if err := a.f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil { return err }
			_, err := tx.Exec(ctx, `UPDATE app.checkout_sessions SET address_snapshot=jsonb_set(address_snapshot,'{accepted_total}',jsonb_build_object('amount_minor',$3::bigint,'currency',$4::text)) WHERE tenant_id=$1 AND id=$2`, a.f.tenant, adopted.CheckoutID, adopted.AmountMinor, adopted.Currency)
			return err
		}); err != nil { return err }
		if err := check.AdmitRealPayment(ctx, request, true); err == nil { t.Error("ADOPT_SOURCE_RED: source guard admitted existing order with a matching receipt") }
		return denied
	}
	_, err := a.f.service.Confirm(checkoutRequestContext(a.f.ctx), a.f.event(adopted, payment.StatusAuthorized))
	if err == nil || adoptCalls != 1 || a.f.provider.captures.Load() != 0 { t.Fatal(fmt.Sprintf("AdoptOrder witness did not reach capture admission: calls=%d captures=%d err=%v", adoptCalls, a.f.provider.captures.Load(), err)) }
}
