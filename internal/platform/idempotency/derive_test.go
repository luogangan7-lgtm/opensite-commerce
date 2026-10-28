// Responsibility: pin internal UUIDv5 replay identities and purpose isolation.
// Dependencies: the shared key helper and UUID standard vectors; CI runs this.
// Invariant: the same purpose/input replays, and tuple boundaries never alias.
package idempotency

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestDerivedKeyUsesPurposeUUIDv5(t *testing.T) {
	// A fixed purpose namespace is UUIDv5(OID, purpose); the input is a JSON tuple.
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte("checkout.order"))
	want := uuid.NewSHA1(namespace, []byte(`["order-id"]`)).String()
	got := DerivedKey("checkout.order", "order-id")
	if got != want || DerivedKey("checkout.order", "order-id") != got {
		t.Fatalf("internal key must be deterministic UUIDv5: got %q want %q", got, want)
	}
	if admitted, err := ValidateKey(http.Header{"Idempotency-Key": {got}}); err != nil || admitted != got {
		t.Fatalf("derived key fails universal admission: %q %v", admitted, err)
	}
	for _, other := range []string{
		DerivedKey("checkout.cancel", "order-id"),
		DerivedKey("checkout.order", "another-order"),
		DerivedKey("checkout.order", "order", "-id"),
		DerivedKey("checkout.order", "order-id", ""),
	} {
		if other == got {
			t.Fatal("purpose or tuple boundary collapsed")
		}
	}
}
