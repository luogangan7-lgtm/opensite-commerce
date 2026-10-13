// These fixtures encode the order lifecycle independently of the Go state gate.
package contract_test

import (
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"testing"
)

func TestStateBoundaries(t *testing.T) {
	tests := []struct {
		from, to contract.State
		ok       bool
	}{
		{"pending_payment", "paid", true}, {"pending_payment", "cancelled", true},
		{"paid", "partially_fulfilled", true}, {"paid", "fulfilled", true}, {"paid", "cancelled", false},
		{"paid", "partially_refunded", true}, {"paid", "refunded", true},
		{"partially_fulfilled", "fulfilled", true}, {"partially_fulfilled", "partially_refunded", true}, {"partially_fulfilled", "refunded", true},
		{"fulfilled", "partially_refunded", true}, {"fulfilled", "refunded", true},
		{"partially_refunded", "partially_refunded", true}, {"partially_refunded", "refunded", true},
		{"pending_payment", "fulfilled", false}, {"pending_payment", "refunded", false},
		{"paid", "pending_payment", false}, {"cancelled", "paid", false}, {"refunded", "paid", false},
		{"fulfilled", "partially_fulfilled", false}, {"paid", "paid", false}, {"", "paid", false}, {"paid", "unknown", false},
	}
	for _, x := range tests {
		t.Run(string(x.from)+"/"+string(x.to), func(t *testing.T) {
			err := contract.ValidateTransition(x.from, x.to)
			if x.ok && err != nil || !x.ok && !errors.Is(err, contract.ErrConflict) {
				t.Errorf("state transition %s -> %s: %v", x.from, x.to, err)
			}
		})
	}
}
