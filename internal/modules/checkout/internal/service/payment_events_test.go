// Responsibility: guard bounded incremental refund allocation independent of PSP.
// Dependencies: checkout snapshot and exact money; caller: CI unit test runner.
// Invariants: repeated small refunds never retract or exceed a component budget.
package service

import (
	"context"
	"errors"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestRefundRemainingComponents(t *testing.T) {
	row := db.AppCheckoutSession{Currency: "USD", AmountMinor: 9, ItemsNetMinor: 5, TaxMinor: 3, ShippingMinor: 1}
	for i, amount := range []int64{4, 1, 1, 3} {
		parts, cumulative, err := refundParts(row, money.Money{Currency: "USD", Amount: amount})
		if err != nil {
			t.Fatal(err)
		}
		if parts[0].Amount+parts[1].Amount+parts[2].Amount != amount {
			t.Fatal("refund allocation does not conserve its delta")
		}
		row.RefundedNetMinor += parts[0].Amount
		row.RefundedTaxMinor += parts[1].Amount
		row.RefundedShippingMinor += parts[2].Amount
		if row.RefundedNetMinor > 5 || row.RefundedTaxMinor > 3 || row.RefundedShippingMinor > 1 {
			t.Fatalf("step %d exceeded original components", i)
		}
		if cumulative != row.RefundedNetMinor+row.RefundedTaxMinor+row.RefundedShippingMinor {
			t.Fatal("cumulative amount disagrees with components")
		}
	}
	if row.RefundedNetMinor != 5 || row.RefundedTaxMinor != 3 || row.RefundedShippingMinor != 1 {
		t.Fatal("full refund does not reverse the original components")
	}
	if _, _, err := refundParts(row, money.Money{Currency: "USD", Amount: 1}); err == nil {
		t.Fatal("accepted excess refund")
	}
}

func TestFactInvalidInputUsesNeutralConflict(t *testing.T) {
	var service *Service
	applied, err := service.ApplyPaymentEvent(context.Background(), nil, payment.VerifiedEvent{})
	var conflict *payment.PaymentError
	if applied || !errors.As(err, &conflict) || conflict.Code != payment.ErrorStateConflict {
		t.Fatalf("invalid fact did not preserve neutral conflict: applied=%v error=%v", applied, err)
	}
}

func TestRefundInvalidDelta(t *testing.T) {
	row := db.AppCheckoutSession{Currency: "USD", AmountMinor: 9, ItemsNetMinor: 5, TaxMinor: 3, ShippingMinor: 1}
	for _, delta := range []money.Money{{Amount: 0, Currency: "USD"}, {Amount: -1, Currency: "USD"}, {Amount: 10, Currency: "USD"}, {Amount: 1, Currency: "EUR"}} {
		if _, _, err := refundParts(row, delta); err == nil {
			t.Fatalf("accepted invalid delta %+v", delta)
		}
	}
}
