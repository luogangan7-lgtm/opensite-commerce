// Package store owns private tenant authorization and API-key SQL transactions.
// Dependencies: sqlc, identity contract and scoped identity adapter; tenant calls.
// Invariants: dedicated app_auth LOGIN validated each transaction, sanitized
// errors, no raw pool escapes; write revalidation uses caller business Queries.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	auth "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Repository struct{ runner *pgxadapter.IdentityRunner }

func Open(ctx context.Context, dsn string) (*Repository, error) {
	runner, err := pgxadapter.OpenAuthorization(ctx, dsn)
	if err != nil {
		return nil, auth.ErrUnavailable
	}
	r := &Repository{runner: runner}
	if err := r.run(ctx, func(context.Context, *db.Queries) error { return nil }); err != nil {
		_ = runner.Close(context.Background())
		return nil, auth.ErrUnavailable
	}
	return r, nil
}
func (r *Repository) Close(ctx context.Context) error { return r.runner.Close(ctx) }
func (r *Repository) run(ctx context.Context, fn func(context.Context, *db.Queries) error) error {
	err := r.runner.RunAuthentication(ctx, func(ctx context.Context, q *db.Queries) error {
		// Only Router's private stamp can select the tenant. Pre-admission
		// lookups/startup remain global and never consume request path values.
		if tenant, e := platform.AuthorizedTenant(ctx); e == nil {
			if e := q.TenantSetContext(ctx, id(tenant)); e != nil {
				return auth.ErrUnavailable
			}
			if e := platform.RevalidateTransaction(ctx, q); e != nil {
				return e
			}
		}
		return fn(ctx, q)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, auth.ErrNotFound) {
		return auth.ErrNotFound
	}
	var failure *apperrors.Error
	if errors.As(err, &failure) && failure != nil {
		return err
	}
	if code, ok := err.(interface{ SQLState() string }); ok {
		switch code.SQLState() {
		case "42501":
			return apperrors.New(403, "forbidden", "API key management denied")
		case "22023":
			return apperrors.New(400, "invalid_api_key", "API key input invalid")
		}
	}
	return auth.ErrUnavailable
}
func id(value string) pgtype.UUID { var v pgtype.UUID; _ = v.Scan(value); return v }
func nullableTime(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}
func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid || value.InfinityModifier != pgtype.Finite {
		return nil
	}
	t := value.Time.UTC()
	return &t
}
func view(id, keyPrefix string, scopes []string, issuedBy string, created, expires, revoked, lastUsed pgtype.Timestamptz) contract.APIKeyView {
	return contract.APIKeyView{ID: id, KeyPrefix: keyPrefix, Scopes: append([]string{}, scopes...), IssuedBy: issuedBy, CreatedAt: created.Time.UTC(), ExpiresAt: timePointer(expires), RevokedAt: timePointer(revoked), LastUsedAt: timePointer(lastUsed)}
}
func (r *Repository) Membership(ctx context.Context, userID, tenantID string) (auth.Membership, error) {
	var result auth.Membership
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		v, e := q.TenantMembership(ctx, db.TenantMembershipParams{UserID: id(userID), TenantID: id(tenantID)})
		if e != nil {
			return e
		}
		result = auth.Membership{UserID: v.UserID.String(), TenantID: v.TenantID.String(), Role: v.Role}
		return nil
	})
	return result, err
}
func (r *Repository) Key(ctx context.Context, keyID string) (auth.APIKey, error) {
	var result auth.APIKey
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		v, e := q.TenantAPIKey(ctx, id(keyID))
		if e != nil {
			return e
		}
		if len(v.KeyHash) != 32 {
			return auth.ErrUnavailable
		}
		result = auth.APIKey{ID: v.ID.String(), TenantID: v.TenantID.String(), UserID: v.IssuedBy.String(), Scopes: append([]string{}, v.Scopes...)}
		copy(result.Hash[:], v.KeyHash)
		return nil
	})
	return result, err
}
func (r *Repository) TouchKey(ctx context.Context, keyID string) error {
	return r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		v, e := q.TenantTouchAPIKey(ctx, id(keyID))
		if e != nil {
			return e
		}
		if !v {
			return auth.ErrNotFound
		}
		return nil
	})
}
func (r *Repository) Revalidate(ctx context.Context, q *db.Queries, tenant string, a auth.TransactionAuthorization) error {
	if q == nil {
		return auth.ErrUnavailable
	}
	valid, e := q.TenantRevalidateAuthorization(ctx, db.TenantRevalidateAuthorizationParams{TenantID: id(tenant), UserID: id(a.UserID), Kind: a.Kind, CredentialID: id(a.CredentialID), Scope: a.Scope})
	if e != nil {
		return auth.ErrUnavailable
	}
	if !valid {
		return auth.ErrNotFound
	}
	return nil
}
func (r *Repository) List(ctx context.Context, tenant, user string) ([]contract.APIKeyView, error) {
	items := []contract.APIKeyView{}
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		m, e := q.TenantMembership(ctx, db.TenantMembershipParams{UserID: id(user), TenantID: id(tenant)})
		if e != nil {
			return e
		}
		if m.Role != "owner" && m.Role != "admin" {
			return auth.ErrNotFound
		}
		rows, e := q.TenantListAPIKeys(ctx, db.TenantListAPIKeysParams{UserID: id(user), TenantID: id(tenant)})
		if e != nil {
			return e
		}
		for _, v := range rows {
			items = append(items, view(v.ID.String(), v.KeyPrefix, v.Scopes, v.IssuedBy.String(), v.CreatedAt, v.ExpiresAt, v.RevokedAt, v.LastUsedAt))
		}
		return nil
	})
	return items, err
}
func (r *Repository) Create(ctx context.Context, tenant, user string, k contract.KeyMaterial) (contract.APIKeyView, error) {
	var result contract.APIKeyView
	err := r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		created, e := q.TenantCreateAPIKey(ctx, db.TenantCreateAPIKeyParams{TenantID: id(tenant), UserID: id(user), ID: id(k.ID), KeyPrefix: k.Prefix, KeyHash: k.Hash[:], Scopes: k.Scopes, ExpiresAt: nullableTime(k.ExpiresAt)})
		if e != nil {
			return e
		}
		v, e := q.TenantGetAPIKeyView(ctx, db.TenantGetAPIKeyViewParams{TenantID: id(tenant), ID: created})
		if e != nil {
			return e
		}
		result = view(v.ID.String(), v.KeyPrefix, v.Scopes, v.IssuedBy.String(), v.CreatedAt, v.ExpiresAt, v.RevokedAt, v.LastUsedAt)
		return nil
	})
	return result, err
}
func (r *Repository) Revoke(ctx context.Context, tenant, user, keyID string) error {
	return r.run(ctx, func(ctx context.Context, q *db.Queries) error {
		valid, e := q.TenantRevokeAPIKey(ctx, db.TenantRevokeAPIKeyParams{TenantID: id(tenant), UserID: id(user), ID: id(keyID)})
		if e != nil {
			return e
		}
		if !valid {
			return auth.ErrNotFound
		}
		return nil
	})
}
