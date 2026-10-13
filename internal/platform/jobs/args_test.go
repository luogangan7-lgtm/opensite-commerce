// Jobs admission tests protect signed tenant provenance before any River work.
// Dependencies: stdlib and this public jobs package; no database is mocked here.
package jobs

import "testing"

func TestSignedTenantAdmission(t *testing.T) {
	s, err := NewSigner([]byte("disposable-test-signing-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.inventoryArgs("11111111-1111-4111-8111-111111111111", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(a); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*InventoryExpireArgs){
		func(a *InventoryExpireArgs) { a.TenantID = "22222222-2222-4222-8222-222222222222" },
		func(a *InventoryExpireArgs) { a.Batch++ },
		func(a *InventoryExpireArgs) { a.Proof = "forged" },
	} {
		b := a
		mutate(&b)
		if err := s.Validate(b); err == nil {
			t.Fatal("forged tenant or payload admitted")
		}
	}
}

func TestJobArgumentsAndSignerRejectInvalid(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("weak signing key accepted")
	}
	s, _ := NewSigner([]byte("disposable-test-signing-key-32bytes"))
	for _, tenant := range []string{"", "bad", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.inventoryArgs(tenant, 100); err == nil {
			t.Fatal("invalid tenant accepted")
		}
	}
	for _, batch := range []int{0, -1, 1001} {
		if _, err := s.inventoryArgs("11111111-1111-4111-8111-111111111111", batch); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
}
