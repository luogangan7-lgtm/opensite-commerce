// Responsibility: pin canonical refund keys and immutable reversal preparation.
// Dependencies: public Payments and Ledger values; caller: CI Go lane.
// Invariant: duplicate delivery identity is absent from the monetary key.
package contract_test

import (
	"reflect"
	"strings"
	"testing"

	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestRefundReversalPreparation(t *testing.T) {
	identity := contract.RefundIdentity{TenantID: "00000000-0000-0000-0000-000000000001", Provider: "stripe", AccountID: "acct_one", RefundID: "re_one"}
	original := ledger.Transaction{TenantID: identity.TenantID, Currency: "USD", SourceType: "order", SourceID: "original", Event: "refund", Entries: []ledger.Entry{
		{AccountID: "00000000-0000-0000-0000-000000000002", Side: ledger.Debit, Amount: money.Money{Amount: 9, Currency: "USD"}},
		{AccountID: "00000000-0000-0000-0000-000000000003", Side: ledger.Credit, Amount: money.Money{Amount: 9, Currency: "USD"}},
	}}
	before := append([]ledger.Entry(nil), original.Entries...)
	reversed, err := identity.Reversal(original)
	if err != nil || reversed.Validate() != nil {
		t.Fatal(err)
	}
	if reversed.SourceType != "payment_refund" || reversed.Event != "posted_to_compensated/v1" || len(reversed.SourceID) != 64 {
		t.Fatalf("bad identity: %+v", reversed)
	}
	for i, e := range reversed.Entries {
		if e.AccountID != original.Entries[i].AccountID || e.Amount != original.Entries[i].Amount || e.Side == original.Entries[i].Side {
			t.Fatal("changed financial content")
		}
	}
	reversed.Entries[0].Amount.Amount = 1
	if !reflect.DeepEqual(original.Entries, before) {
		t.Fatal("reversal mutated original")
	}
	replay, err := identity.Reversal(original)
	if err != nil || replay.SourceID != reversed.SourceID {
		t.Fatal("unstable replay")
	}
	for _, change := range []func(*contract.RefundIdentity){func(r *contract.RefundIdentity) { r.AccountID = "acct_two" }, func(r *contract.RefundIdentity) { r.RefundID = "re_two" }, func(r *contract.RefundIdentity) { r.Provider = "fake" }} {
		other := identity
		change(&other)
		key, err := other.ReversalSourceID()
		if err != nil || key == reversed.SourceID {
			t.Fatal("account/provider/refund alias")
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 201), "re\n_bad", "退款"} {
		other := identity
		other.RefundID = bad
		if _, err := other.ReversalSourceID(); err == nil {
			t.Fatal("invalid refund ID accepted")
		}
	}
	original.TenantID = "00000000-0000-0000-0000-000000000004"
	if _, err := identity.Reversal(original); err == nil {
		t.Fatal("cross tenant reversal accepted")
	}
}
