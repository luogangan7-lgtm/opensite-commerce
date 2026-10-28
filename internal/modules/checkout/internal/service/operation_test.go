// Responsibility: pin provider retry identity across process restarts.
// Dependencies: persisted Checkout rows and operation; caller: Foundation Go lanes.
// Invariant: the same row/action never creates a different provider command key.
package service

import (
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
)

func TestProviderOperationKeyIsStableForRowAndAction(t *testing.T) {
	row := db.AppCheckoutSession{ID: store.UUID("11111111-1111-4111-8111-111111111111")}
	const want = "a54be9ef-e7ca-5830-982f-60afc64874ef"
	for range 3 {
		if got := operation(row, "capture").IdempotencyKey; got != want {
			t.Fatalf("provider retry key changed: got %q want %q", got, want)
		}
	}
	if operation(row, "cancel").IdempotencyKey == want {
		t.Fatal("different provider actions share a command identity")
	}
	row.ID = store.UUID("22222222-2222-4222-8222-222222222222")
	if operation(row, "capture").IdempotencyKey == want {
		t.Fatal("different persisted rows share a provider command identity")
	}
}
