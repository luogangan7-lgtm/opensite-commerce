// Responsibility: fail-closed provider authority and typed Tenant port wiring.
// Dependencies: production Payments gate with transaction-query test doubles.
// Invariants: only test code can witness mapped provider settlement today.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	tenant "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
)

type settlementFake struct {
	outcome contract.SettlementOutcome
	err     error
	q       *db.Queries
	tenant  string
}

func (f *settlementFake) Settlement(_ context.Context, q *db.Queries, tenantID string) (contract.SettlementOutcome, error) {
	f.q, f.tenant = q, tenantID
	return f.outcome, f.err
}

type deletionQueries struct{ db.DBTX }

type deletionRow struct{}

func (deletionQueries) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return deletionRow{}
}

func (deletionRow) Scan(values ...interface{}) error {
	*values[0].(*[]byte) = []byte(`{"payment_refund":0,"refund_reconciliation":0}`)
	return nil
}

func TestMerchantDeletionProviderAuthority(t *testing.T) {
	ctx := context.Background()
	q := db.New(deletionQueries{})
	const tenantID = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name     string
		provider contract.ProviderSettlement
		known    bool
		count    int64
	}{
		{"default", nil, false, 0},
		{"unknown", &settlementFake{outcome: contract.SettlementUnknown}, false, 0},
		{"unsettled", &settlementFake{outcome: contract.SettlementUnsettled}, true, 1},
		{"settled-test-only", &settlementFake{outcome: contract.SettlementSettled}, true, 0},
		{"invalid", &settlementFake{outcome: "guessed"}, false, 0},
		{"failed", &settlementFake{outcome: contract.SettlementSettled, err: errors.New("unavailable")}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewDeletionObligations(tc.provider)
			items, err := tenant.CollectDeletionObligations(ctx, q, tenantID, []tenant.ObligationPort{gate})
			if err != nil {
				t.Fatal(err)
			}
			counts := tenant.ObligationCounts(items)
			for _, category := range []tenant.ObligationCategory{tenant.PendingPayout, tenant.UnsettledProviderBalance} {
				count, known := counts[category]
				if known != tc.known || count != tc.count {
					t.Fatalf("MDEL04_PROVIDER_GATE %s: %v", category, counts)
				}
			}
			if counts[tenant.PaymentRefund] != 0 || counts[tenant.RefundReconciliation] != 0 {
				t.Fatal("MDEL04_INTERNAL_SETTLEMENT_WIRING")
			}
			if f, ok := tc.provider.(*settlementFake); ok && (f.q != q || f.tenant != tenantID) {
				t.Fatal("MDEL04_PROVIDER_TRANSACTION_BINDING")
			}
		})
	}
	if _, ok := NewDeletionObligations(nil).provider.(UnknownProviderSettlement); !ok {
		t.Fatal("MDEL04_DEFAULT_ADAPTER")
	}
}
