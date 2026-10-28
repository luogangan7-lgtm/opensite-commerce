// Responsibility: define the consumer-owned financial/hold admission port.
// Dependencies: transaction-scoped generated queries and canonical UUID validation.
// Callers: DataRights composes owner ports after taking Tenant's exclusive fence.
// Invariant: missing/failed/unknown authority is blocked; no UI status, network IO,
// application clock or another module's private store is an obligation witness.
package contract

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type ObligationCategory string

const (
	PaidUnfulfilled          ObligationCategory = "paid_unfulfilled"
	FulfillmentReturn        ObligationCategory = "fulfillment_return"
	PaymentRefund            ObligationCategory = "payment_refund"
	CheckoutCapture          ObligationCategory = "checkout_capture"
	RefundReconciliation     ObligationCategory = "refund_reconciliation"
	PendingPayout            ObligationCategory = "pending_payout"
	UnsettledProviderBalance ObligationCategory = "unsettled_provider_balance"
	LegalHold                ObligationCategory = "legal_hold"
)

// ObligationObject is returned only to the authorized owner; it is not durable
// lifecycle metadata. Owner ports must prove tenant ownership of every ID.
type ObligationObject struct {
	ID             string `json:"id"`
	ResolutionPath string `json:"resolution_path"`
}

type Obligation struct {
	Category ObligationCategory `json:"category"`
	Known    bool               `json:"known"`
	Count    int64              `json:"count"`
	Objects  []ObligationObject `json:"objects,omitempty"`
}

// ObligationPort must read its own authoritative store using q, in the fence
// holder's transaction. Provider/hold authority absent from the local owner
// contract returns Known=false; no payout/refund automation belongs here.
type ObligationPort interface {
	DeletionObligations(context.Context, *db.Queries, string) ([]Obligation, error)
}

// CollectDeletionObligations is fail-closed for incomplete composition. Each
// category has exactly one responsible owner. A failed owner cannot contribute
// a partially trusted zero count. SQL receives only the resulting count map;
// links and object IDs never enter replay bodies or lifecycle audit cells.
func CollectDeletionObligations(ctx context.Context, q *db.Queries, tenant string, ports []ObligationPort) ([]Obligation, error) {
	if q == nil || !tenanttx.ValidTenantID(tenant) {
		return nil, errors.New("invalid obligation transaction")
	}
	categories := []ObligationCategory{PaidUnfulfilled, FulfillmentReturn, PaymentRefund, CheckoutCapture, RefundReconciliation, PendingPayout, UnsettledProviderBalance, LegalHold}
	values := make(map[ObligationCategory]Obligation, len(categories))
	for _, category := range categories {
		values[category] = Obligation{Category: category}
	}
	seen := make(map[ObligationCategory]bool, len(categories))
	for _, port := range ports {
		if port == nil {
			continue
		}
		items, err := port.DeletionObligations(ctx, q, tenant)
		if err != nil {
			return nil, errors.New("obligation authority unavailable")
		}
		for _, item := range items {
			if _, ok := values[item.Category]; !ok || seen[item.Category] || item.Count < 0 || len(item.Objects) > 16 || int64(len(item.Objects)) > item.Count {
				return nil, errors.New("invalid obligation witness")
			}
			for _, object := range item.Objects {
				if !tenanttx.ValidTenantID(object.ID) || len(object.ResolutionPath) > 512 || !strings.HasPrefix(object.ResolutionPath, "/admin/t/"+tenant+"/") || strings.ContainsAny(object.ResolutionPath, "%?#\\\r\n") || path.Clean(object.ResolutionPath) != object.ResolutionPath {
					return nil, errors.New("invalid obligation object")
				}
			}
			seen[item.Category] = true
			values[item.Category] = item
		}
	}
	result := make([]Obligation, 0, len(categories))
	for _, category := range categories {
		result = append(result, values[category])
	}
	return result, nil
}

// ObligationCounts is deliberately incapable of converting unknown authority
// into a zero witness. Missing categories stay missing and SQL blocks admission.
func ObligationCounts(items []Obligation) map[ObligationCategory]int64 {
	result := make(map[ObligationCategory]int64, len(items))
	for _, item := range items {
		if item.Known && item.Count >= 0 {
			result[item.Category] = item.Count
		}
	}
	return result
}
