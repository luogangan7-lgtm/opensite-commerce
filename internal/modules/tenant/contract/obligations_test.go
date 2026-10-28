// Responsibility: reject incomplete, spoofed and unbounded obligation witnesses.
// Dependencies: public obligation port; CI Go lane. No database is simulated as
// settled: fake ports exercise validation only; PG tests own lifecycle admission.
package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type obligationFixture struct {
	items []Obligation
	err   error
}

func (f obligationFixture) DeletionObligations(context.Context, *db.Queries, string) ([]Obligation, error) {
	return f.items, f.err
}

func TestObligationAuthorityCannotDefaultToSettled(t *testing.T) {
	ctx := context.Background()
	tenant := "12345678-1234-1234-1234-123456789abc"
	items, err := CollectDeletionObligations(ctx, &db.Queries{}, tenant, nil)
	if err != nil || len(items) != 8 || len(ObligationCounts(items)) != 0 {
		t.Fatalf("unknown authorities must block: %v %v", items, err)
	}
	for _, category := range []ObligationCategory{PendingPayout, UnsettledProviderBalance, LegalHold} {
		items, err = CollectDeletionObligations(ctx, &db.Queries{}, tenant, []ObligationPort{obligationFixture{items: []Obligation{{Category: category, Known: true, Count: 1}}}})
		if err != nil || ObligationCounts(items)[category] != 1 {
			t.Fatalf("lost blocker %s: %v %v", category, items, err)
		}
	}
	for _, f := range []obligationFixture{
		{err: errors.New("provider not mapped")},
		{items: []Obligation{{Category: "unknown", Known: true}}},
		{items: []Obligation{{Category: PendingPayout, Count: -1}}},
		{items: []Obligation{{Category: PendingPayout}, {Category: PendingPayout}}},
		{items: []Obligation{{Category: PendingPayout, Known: true, Count: 1, Objects: []ObligationObject{{ID: tenant, ResolutionPath: "/admin/t/another-tenant/payouts"}}}}},
	} {
		if _, err := CollectDeletionObligations(ctx, &db.Queries{}, tenant, []ObligationPort{f}); err == nil {
			t.Fatalf("accepted invalid authority: %+v", f)
		}
	}
}
