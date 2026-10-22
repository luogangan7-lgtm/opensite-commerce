//go:build pgx

// Responsibilities: prove fulfillment/refund authority at the SQL write fence.
// Dependencies: disposable login PG fixture and existing app_auth capabilities.
// Invariants: DB-clock credentials, exact key scopes, current roles and tenant
// binding are required; rejected issuance creates no key or widened authority.
package identity

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestC20aWriteScopesPostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "c20a-scopes@example.test")
	var tenant, other, session string
	for _, target := range []*string{&tenant, &other} {
		if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C20a scopes') RETURNING id::text`).Scan(target); err != nil {
			t.Fatal("scope tenant fixture failed", safeCode(err))
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$3,'owner'),($2,$3,'owner')`, tenant, other, user); err != nil {
		t.Fatal("scope membership fixture failed", safeCode(err))
	}
	if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text`, user, c02cKeyHash(t)).Scan(&session); err != nil {
		t.Fatal("scope session fixture failed", safeCode(err))
	}
	check := func(t *testing.T, target, kind, credential, scope string, want bool) {
		t.Helper()
		var got bool
		err := admin.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_runtime`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, target); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT app.auth_revalidate_tenant($1,$2,$3,$4,$5)`, target, user, kind, credential, scope).Scan(&got)
		})
		if err != nil || got != want {
			t.Error("scope write fence mismatch", kind, scope, got, want, safeCode(err))
		}
	}
	for _, role := range []string{"owner", "admin", "catalog_editor", "content_editor", "viewer"} {
		t.Run(role, func(t *testing.T) {
			if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, tenant, user, role); err != nil {
				t.Fatal("scope role fixture failed", safeCode(err))
			}
			for _, scope := range []string{"fulfillment:write", "returns:write", "unknown:scope", "*"} {
				t.Run(scope, func(t *testing.T) {
					allowed := (role == "owner" || role == "admin") && (scope == "fulfillment:write" || scope == "returns:write")
					check(t, tenant, "session", session, scope, allowed)
					key := uuid.NewString()
					var created string
					err := auth.QueryRow(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'c20a',$4,$5,NULL)::text`, tenant, user, key, c02cKeyHash(t), []string{scope}).Scan(&created)
					if allowed {
						if err != nil || created != key {
							t.Error("explicit owner/admin scope issuance refused", safeCode(err))
							return
						}
						check(t, tenant, "api_key", key, scope, true)
						check(t, other, "api_key", key, scope, false)
						otherScope := "fulfillment:write"
						if scope == otherScope {
							otherScope = "returns:write"
						}
						check(t, tenant, "api_key", key, otherScope, false)
						if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
							t.Fatal("scope downgrade failed", safeCode(err))
						}
						check(t, tenant, "api_key", key, scope, false)
						if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, tenant, user, role); err != nil {
							t.Fatal("scope role restore failed", safeCode(err))
						}
					} else {
						wantCode := "42501"
						if role == "owner" || role == "admin" {
							wantCode = "22023"
						}
						if safeCode(err) != wantCode {
							t.Error("invalid scope issuance did not fail closed", safeCode(err), wantCode)
						}
						var count int
						if err := admin.QueryRow(ctx, `SELECT count(*) FROM app.api_keys WHERE id=$1`, key).Scan(&count); err != nil || count != 0 {
							t.Error("denied issuance mutated keys", count, safeCode(err))
						}
					}
				})
			}
		})
	}
}
