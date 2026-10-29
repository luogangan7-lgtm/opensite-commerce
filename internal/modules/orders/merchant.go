// Responsibility: dormant Orders ports for merchant deletion composition.
// Dependencies: generated transaction queries and Tenant's consumer-owned port.
// Callers: DataRights under its common fence; no pool, routes or worker grants.
// Invariants: real quantity authority, bounded tenant links, exact epoch scrub.
package orders

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	tenant "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type MerchantDeletion struct{}

var _ tenant.ObligationPort = MerchantDeletion{}

// DeletionObligations shares the coordinator's exclusive lifecycle transaction.
// Other obligation categories remain unknown until their owner ports are wired.
func (MerchantDeletion) DeletionObligations(ctx context.Context, q *db.Queries, tenantID string) ([]tenant.Obligation, error) {
	if q == nil || !tenanttx.ValidTenantID(tenantID) {
		return nil, errors.New("Orders obligation authority unavailable")
	}
	var t pgtype.UUID
	_ = t.Scan(tenantID)
	raw, err := q.GetOrdersMerchantObligations(ctx, t)
	if err != nil {
		return nil, errors.New("Orders obligation authority unavailable")
	}
	var witness struct {
		Count     *int64   `json:"count"`
		ObjectIDs []string `json:"object_ids"`
	}
	if err := json.Unmarshal(raw, &witness); err != nil || witness.Count == nil || witness.ObjectIDs == nil || *witness.Count < int64(len(witness.ObjectIDs)) || len(witness.ObjectIDs) > 16 {
		return nil, errors.New("Orders obligation authority unavailable")
	}
	item := tenant.Obligation{Category: tenant.PaidUnfulfilled, Known: true, Count: *witness.Count}
	for _, value := range witness.ObjectIDs {
		if !tenanttx.ValidTenantID(value) {
			return nil, errors.New("Orders obligation authority unavailable")
		}
		item.Objects = append(item.Objects, tenant.ObligationObject{ID: value, ResolutionPath: "/admin/t/" + tenantID + "/orders/" + value})
	}
	return []tenant.Obligation{item}, nil
}

// EraseMerchant must commit with the coordinator's request/version/digest/cursor
// checkpoint CAS. A failed checkpoint rolls back this entire owner mutation.
func (MerchantDeletion) EraseMerchant(ctx context.Context, q *db.Queries, tenantID, requestID string, epoch int64) (int64, error) {
	if q == nil || !tenanttx.ValidTenantID(tenantID) || !tenanttx.ValidTenantID(requestID) || epoch < 0 {
		return 0, errors.New("Orders erasure denied")
	}
	var t, r pgtype.UUID
	_ = t.Scan(tenantID)
	_ = r.Scan(requestID)
	n, err := q.EraseOrdersMerchant(ctx, db.EraseOrdersMerchantParams{TenantID: t, RequestID: r, Epoch: epoch})
	if err != nil {
		return 0, errors.New("Orders erasure denied")
	}
	return n, nil
}
