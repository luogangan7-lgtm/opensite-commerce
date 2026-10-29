// Responsibility: pin fail-closed public merchant owner-port admission.
// Dependencies: Orders public port; SQL behavior is proved separately on PG18.
// Invariant: invalid contexts cannot be reported as known-zero obligations.
package orders

import (
	"context"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

func TestMerchantDeletionRejectsUntrustedContext(t *testing.T) {
	port := MerchantDeletion{}
	ctx := context.Background()
	if _, err := port.DeletionObligations(ctx, nil, "019a0000-0000-7000-8000-000000000001"); err == nil {
		t.Fatal("missing transaction admitted")
	}
	if _, err := port.DeletionObligations(ctx, &db.Queries{}, "invalid"); err == nil {
		t.Fatal("invalid tenant admitted")
	}
	if _, err := port.EraseMerchant(ctx, &db.Queries{}, "invalid", "invalid", 0); err == nil {
		t.Fatal("invalid erasure admitted")
	}
}
