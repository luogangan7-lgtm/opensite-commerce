// Responsibility: persist Identity proofs and read receipt-only authorization.
// Dependencies: generated queries and the existing bounded auth runner.
// Invariants: tenant GUC and explicit predicates, no raw secret storage/errors.
package store

import (
	"context"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

func (r *LoginRepository) MerchantIdentifier(ctx context.Context, user string) (string, error) {
	var result string
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		u, err := uuid(user)
		if err != nil {
			return login.ErrCredentials
		}
		value, err := q.MerchantStepUpIdentifier(ctx, u)
		if err != nil || !value.Valid {
			return login.ErrCredentials
		}
		result = value.String
		return nil
	})
	return result, err
}

func (r *LoginRepository) IssueMerchantProof(ctx context.Context, tenant string, actor login.Actor, action string, digest [32]byte, verified string) (time.Time, error) {
	var result time.Time
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		t, err := uuid(tenant)
		if err != nil {
			return login.ErrCredentials
		}
		u, _ := uuid(actor.UserID())
		s, _ := uuid(actor.SessionID())
		if err := q.TenantSetContext(ctx, t); err != nil {
			return err
		}
		if err := q.LoginSetTransactionIdle(ctx, int64(r.idle / time.Second)); err != nil {
			return err
		}
		value, err := q.MerchantIssueProof(ctx, db.MerchantIssueProofParams{TenantID: t, UserID: u, SessionID: s, Action: action, ProofDigest: digest[:], VerifiedHash: verified})
		if err != nil {
			return login.ErrCredentials
		}
		result = value.Time
		return nil
	})
	return result, err
}

func (r *LoginRepository) MerchantReceipt(ctx context.Context, tenant, request string, actor login.Actor) (bool, error) {
	var result bool
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		t, _ := uuid(tenant)
		req, _ := uuid(request)
		u, _ := uuid(actor.UserID())
		s, _ := uuid(actor.SessionID())
		if err := q.TenantSetContext(ctx, t); err != nil {
			return err
		}
		if err := q.LoginSetTransactionIdle(ctx, int64(r.idle / time.Second)); err != nil {
			return err
		}
		var err error
		result, err = q.MerchantReceiptAllowed(ctx, db.MerchantReceiptAllowedParams{TenantID: t, RequestID: req, UserID: u, SessionID: s})
		return err
	})
	return result, err
}
