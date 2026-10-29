// Responsibility: dormant Payments deletion obligations and registry account scrub.
// Dependencies: Payments SQL, its provider authority port and Tenant's consumer port.
// Callers: bootstrap and future DataRights coordinator in the fence-holder's tx.
// Invariants: internal settlement cannot imply provider settlement; no money/IO.
package payments

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	tenant "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

// UnknownProviderSettlement is the ONLY shipped provider settlement adapter.
// PAYOUT-AUTHORITY-UNMAPPED: there is no mapped provider payout/balance authority.
// It returns unknown even for tenants without an enabled provider account.
type UnknownProviderSettlement struct{}

func (UnknownProviderSettlement) Settlement(context.Context, *db.Queries, string) (contract.SettlementOutcome, error) {
	return contract.SettlementUnknown, nil
}

type DeletionGate struct {
	provider contract.ProviderSettlement
}

var _ tenant.ObligationPort = (*DeletionGate)(nil)

func NewDeletionObligations(provider contract.ProviderSettlement) *DeletionGate {
	if provider == nil {
		provider = UnknownProviderSettlement{}
	}
	return &DeletionGate{provider: provider}
}

func (g *DeletionGate) DeletionObligations(ctx context.Context, q *db.Queries, tenantID string) ([]tenant.Obligation, error) {
	if g == nil || q == nil || !tenanttx.ValidTenantID(tenantID) {
		return nil, errors.New("Payments obligation authority unavailable")
	}
	raw, err := q.PaymentsDeletionObligations(ctx, id(tenantID))
	if err != nil {
		return nil, errors.New("Payments obligation authority unavailable")
	}
	var counts struct {
		Payment *int64 `json:"payment_refund"`
		Case    *int64 `json:"refund_reconciliation"`
	}
	if json.Unmarshal(raw, &counts) != nil || counts.Payment == nil || counts.Case == nil || *counts.Payment < 0 || *counts.Case < 0 {
		return nil, errors.New("Payments obligation witness invalid")
	}
	outcome := contract.SettlementUnknown
	if g.provider != nil {
		value, providerErr := g.provider.Settlement(ctx, q, tenantID)
		if providerErr == nil {
			outcome = value
		}
	}
	// An invalid enum is also unknown. Unsettled proves a blocker, never an
	// invented balance/amount: each category has one outstanding authority gate.
	known := outcome == contract.SettlementSettled || outcome == contract.SettlementUnsettled
	var blockers int64
	if outcome == contract.SettlementUnsettled {
		blockers = 1
	}
	return []tenant.Obligation{
		{Category: tenant.PaymentRefund, Known: true, Count: *counts.Payment},
		{Category: tenant.RefundReconciliation, Known: true, Count: *counts.Case},
		{Category: tenant.PendingPayout, Known: known, Count: blockers},
		{Category: tenant.UnsettledProviderBalance, Known: known, Count: blockers},
	}, nil
}

// EraseMerchantRecords implements payments.erase's sole erase cell (enabled).
// All other Payments registry cells are retained unchanged. Coordinator writes
// its work-item checkpoint in this SAME transaction; no application grant yet.
func EraseMerchantRecords(ctx context.Context, q *db.Queries, tenantID, request string, epoch int64) (int64, error) {
	if q == nil || !tenanttx.ValidTenantID(tenantID) || !tenanttx.ValidTenantID(request) || epoch < 0 {
		return 0, errors.New("Payments erasure denied")
	}
	return q.PaymentsEraseMerchantRecords(ctx, db.PaymentsEraseMerchantRecordsParams{TenantID: id(tenantID), RequestID: id(request), Epoch: epoch})
}
