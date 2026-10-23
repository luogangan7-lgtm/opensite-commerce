// Responsibility: preserve neutral conflicts at the fact boundary.
// Dependencies: payment contract; caller: CI unit test runner.
// Invariant: invalid facts are quarantinable without exposing provider details.
package service

import (
	"context"
	"errors"
	"testing"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestFactInvalidInputUsesNeutralConflict(t *testing.T) {
	var service *Service
	applied, err := service.ApplyPaymentEvent(context.Background(), nil, payment.VerifiedEvent{})
	var conflict *payment.PaymentError
	if applied || !errors.As(err, &conflict) || conflict.Code != payment.ErrorStateConflict {
		t.Fatalf("invalid fact did not preserve neutral conflict: applied=%v error=%v", applied, err)
	}
}
