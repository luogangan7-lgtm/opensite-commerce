// Responsibility: expose dormant merchant-deletion Identity owner ports.
// Dependencies: private transport and typed transaction queries; DataRights calls.
// Invariants: caller retains the common tenant fence/transaction, proof is
// digest-only and single-use; this file registers no routes or worker grants.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

func (m *LoginModule) MerchantDeletionStepUpRoute() platform.Route {
	return m.handler.MerchantStepUpRoute()
}

// MerchantDeletionReceiptRoute composes the future DataRights metadata reader;
// its transaction uses the Router's sealed receipt revalidation callback.
func (m *LoginModule) MerchantDeletionReceiptRoute(metadata http.Handler) platform.Route {
	return m.handler.MerchantReceiptRoute(metadata)
}

// ConsumeMerchantProof must share the request/cancel transition transaction.
// The coordinator must check live authorized idempotency replay first; a replay
// must not consume again. There is no standalone consumption pool/capability.
func ConsumeMerchantProof(ctx context.Context, q *db.Queries, tenant string, actor AuthorizedActor, action, raw string) error {
	if q == nil || !tenanttx.ValidTenantID(tenant) || actor.UserID() == "" || actor.SessionID() == "" || (action != "request" && action != "cancel") {
		return errors.New("merchant proof denied")
	}
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(bytes) != 32 || len(raw) != 43 {
		return errors.New("merchant proof denied")
	}
	digest := sha256.Sum256(bytes)
	var t, u, s pgtype.UUID
	_ = t.Scan(tenant)
	_ = u.Scan(actor.UserID())
	_ = s.Scan(actor.SessionID())
	if err := q.MerchantConsumeProof(ctx, db.MerchantConsumeProofParams{TenantID: t, UserID: u, SessionID: s, Action: action, ProofDigest: digest[:]}); err != nil {
		return errors.New("merchant proof denied")
	}
	return nil
}

// EraseMerchantProofs is the identity.erase work item. Global Identity tables
// remain excluded; Tenant's own handler detaches memberships and deletes keys.
// The coordinator persists this result/checkpoint in the same transaction.
func EraseMerchantProofs(ctx context.Context, q *db.Queries, tenant, request string, epoch int64) (int64, error) {
	if q == nil || !tenanttx.ValidTenantID(tenant) || !tenanttx.ValidTenantID(request) || epoch < 0 {
		return 0, errors.New("Identity erasure denied")
	}
	var t, r pgtype.UUID
	_ = t.Scan(tenant)
	_ = r.Scan(request)
	n, err := q.MerchantEraseProofs(ctx, db.MerchantEraseProofsParams{TenantID: t, RequestID: r, Epoch: epoch})
	if err != nil {
		return 0, errors.New("Identity erasure denied")
	}
	return n, nil
}
