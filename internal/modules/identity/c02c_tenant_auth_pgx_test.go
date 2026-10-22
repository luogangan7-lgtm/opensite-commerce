//go:build pgx

// Responsibilities: prove tenant API-key capabilities and write-time authority.
// Dependencies: existing disposable login PG fixtures and identity pgxadapter.
// Invariants: no new production API required for RED; credentials are random,
// real app_auth/app_runtime roles execute SQL, and DB time controls validity.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	authcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func c02cAuthDB(t *testing.T, ctx context.Context, dsn string) *testLoginDB {
	t.Helper()
	runner, err := identityadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		t.Fatal("C02c auth fixture unavailable")
	}
	t.Cleanup(func() { _ = runner.Close(context.Background()) })
	return &testLoginDB{runner: runner}
}

func c02cKeyHash(t *testing.T) []byte {
	t.Helper()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal("C02c fixture entropy unavailable")
	}
	hash := sha256.Sum256(secret[:])
	return hash[:]
}

// Activity receipts use DB time; a throttled touch must not claim a renewal.
func TestC02cSessionActivityThrottlePostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "c02c-activity@example.test")
	for _, probe := range []struct {
		name string
		idle int64
		age  string
		due  bool
	}{
		{"recent_session_not_rewritten", 1800, "0.1 seconds", false},
		{"short_idle_uses_database_clock", 4, "2 seconds", true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			hash := c02cKeyHash(t)
			var before time.Time
			if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at)
 VALUES($1,$2,clock_timestamp()-interval '1 minute',clock_timestamp()-$3::interval,clock_timestamp()+interval '1 hour') RETURNING last_seen_at`, user, hash, probe.age).Scan(&before); err != nil {
				t.Fatal("C02c activity session fixture failed", safeCode(err))
			}
			var result db.LoginLookupSessionRow
			callerTime := before.Add(24 * time.Hour)
			if probe.due {
				callerTime = before.Add(-24 * time.Hour)
			}
			err := auth.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result, err = db.New(tx).LoginLookupSession(ctx, db.LoginLookupSessionParams{
					TokenHash: hash, IdleSeconds: probe.idle,
					At: pgtype.Timestamptz{Time: callerTime, Valid: true},
				})
				return err
			})
			if err != nil {
				t.Fatal("C02c valid activity session refused", safeCode(err))
			}
			if probe.due {
				if !result.LastSeenAt.Time.After(before) {
					t.Fatal("C02c short-idle activity used caller clock instead of DB clock")
				}
			} else if !result.LastSeenAt.Time.Equal(before) {
				t.Fatal("C02c recent session activity bypassed throttle")
			}
			var unchanged bool
			if err := admin.QueryRow(ctx, `SELECT last_seen_at=$2 FROM app.auth_sessions WHERE token_hash=$1`, hash, result.LastSeenAt.Time).Scan(&unchanged); err != nil || !unchanged {
				t.Fatal("C02c session receipt differs from durable activity")
			}
		})
	}
}

func TestC02cAPIKeyActivityThrottlePostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "c02c-key-activity@example.test")
	var tenant string
	if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C02c activity') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal("C02c activity tenant fixture failed")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, tenant, user); err != nil {
		t.Fatal("C02c activity membership fixture failed")
	}
	for _, mode := range []string{"recent", "due", "revoked_recent", "expired_recent"} {
		t.Run(mode, func(t *testing.T) {
			id := uuid.NewString()
			var before time.Time
			if err := admin.QueryRow(ctx, `INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,last_used_at,revoked_at,expires_at)
 VALUES($1,$2,$3,'activity',$4,clock_timestamp()-CASE WHEN $5='due' THEN interval '61 seconds' ELSE interval '0.1 seconds' END,
 CASE WHEN $5='revoked_recent' THEN clock_timestamp() END,CASE WHEN $5='expired_recent' THEN clock_timestamp()-interval '1 second' END)
 RETURNING last_used_at`, tenant, id, user, c02cKeyHash(t), mode).Scan(&before); err != nil {
				t.Fatal("C02c key activity fixture failed", safeCode(err))
			}
			var allowed bool
			if err := auth.QueryRow(ctx, `SELECT app.auth_touch_api_key($1)`, id).Scan(&allowed); err != nil {
				t.Fatal("C02c key activity lookup failed", safeCode(err))
			}
			if allowed != (mode == "recent" || mode == "due") {
				t.Fatal("C02c activity throttle changed credential validity", mode)
			}
			var after time.Time
			if err := admin.QueryRow(ctx, `SELECT last_used_at FROM app.api_keys WHERE id=$1`, id).Scan(&after); err != nil {
				t.Fatal("C02c key activity receipt failed")
			}
			if mode == "due" {
				if !after.After(before) {
					t.Fatal("C02c due key activity not advanced")
				}
			} else if !after.Equal(before) {
				t.Fatal("C02c recent or denied key activity was rewritten", mode)
			}
		})
	}
}

func TestC02cAPIKeyListBoundPostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "c02c-list-bound@example.test")
	var tenant string
	if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C02c list bound') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal("C02c bounded-list tenant fixture failed")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, tenant, user); err != nil {
		t.Fatal("C02c bounded-list membership fixture failed")
	}
	var atDB time.Time
	if err := admin.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&atDB); err != nil {
		t.Fatal("C02c bounded-list DB clock unavailable")
	}
	for n := 1; n <= 105; n++ {
		if _, err := admin.Exec(ctx, `INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,created_at)
 VALUES($1,$2,$3,$4,$5,$6)`, tenant, uuid.NewString(), user, strconv.Itoa(n), c02cKeyHash(t), atDB.Add(-time.Duration(n)*time.Second)); err != nil {
			t.Fatal("C02c bounded-list key fixture failed", safeCode(err))
		}
	}
	var userID, tenantID pgtype.UUID
	if userID.Scan(user) != nil || tenantID.Scan(tenant) != nil {
		t.Fatal("C02c bounded-list fixture IDs invalid")
	}
	err := auth.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		items, err := db.New(tx).TenantListAPIKeys(ctx, db.TenantListAPIKeysParams{UserID: userID, TenantID: tenantID})
		if err == nil && (len(items) != 100 || items[0].KeyPrefix != "1" || items[99].KeyPrefix != "100") {
			t.Error("C02c key list must return exactly the newest 100 keys", len(items))
		}
		return err
	})
	if err != nil {
		t.Fatal("C02c bounded-list query failed", safeCode(err))
	}
}

func TestC02cAuthorizationCapabilitiesPostgres(t *testing.T) {
	ctx, admin, _, _ := setupLoginPG(t)
	t.Run("database_last_used_at", func(t *testing.T) {
		var valid bool
		err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.api_keys'::regclass AND attname='last_used_at' AND atttypid='timestamptz'::regtype AND NOT attnotnull AND NOT attisdropped)`).Scan(&valid)
		if err != nil || !valid {
			t.Fatal("C02c API-key DB last_used_at column missing")
		}
	})
	for _, signature := range []string{
		"app.auth_create_api_key(uuid,uuid,uuid,text,bytea,text[],timestamp with time zone)",
		"app.auth_revoke_api_key(uuid,uuid,uuid)",
		"app.auth_touch_api_key(uuid)",
		"app.auth_revalidate_tenant(uuid,uuid,text,uuid,text)",
	} {
		t.Run(signature, func(t *testing.T) {
			var valid bool
			err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_proc WHERE oid=to_regprocedure($1) AND prosecdef AND proowner='app_credential_writer'::regrole AND proconfig @> ARRAY['search_path=pg_catalog, app, pg_temp'])`, signature).Scan(&valid)
			if err != nil || !valid {
				t.Fatal("C02c restricted definer capability missing", signature)
			}
		})
	}
}

func TestC02cAPIKeyLifecyclePostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "c02c-owner@example.test")
	viewer := seedLoginPG(t, ctx, admin, "c02c-viewer@example.test")
	var tenant, other string
	for _, target := range []*string{&tenant, &other} {
		if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C02c test tenant') RETURNING id::text`).Scan(target); err != nil {
			t.Fatal("C02c tenant fixture failed")
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$3,'owner'),($2,$3,'admin'),($1,$4,'viewer')`, tenant, other, user, viewer); err != nil {
		t.Fatal("C02c membership fixture failed")
	}
	create := func(t *testing.T, actor string, scopes []string) string {
		t.Helper()
		id := uuid.NewString()
		var created string
		err := auth.QueryRow(ctx, `SELECT app.auth_create_api_key($1,$2,$3,$4,$5,$6,NULL)::text`, tenant, actor, id, id[:8], c02cKeyHash(t), scopes).Scan(&created)
		if err != nil || created != id {
			t.Fatal("C02c API-key creation capability unavailable", safeCode(err))
		}
		return id
	}
	var key string
	t.Run("owner_create", func(t *testing.T) { key = create(t, user, []string{"catalog:read"}) })
	if key == "" {
		return
	}
	t.Run("no_arbitrary_key_write", func(t *testing.T) {
		if _, err := auth.Exec(ctx, `UPDATE app.api_keys SET scopes=ARRAY['catalog:write'] WHERE id=$1`, key); safeCode(err) != "42501" {
			t.Fatal("C02c auth role admitted arbitrary key UPDATE", safeCode(err))
		}
		if _, err := auth.Exec(ctx, `INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash) VALUES($1,$2,$3,'forged',$4)`, tenant, uuid.NewString(), user, c02cKeyHash(t)); safeCode(err) != "42501" {
			t.Fatal("C02c auth role admitted arbitrary key INSERT", safeCode(err))
		}
	})
	t.Run("viewer_cannot_issue", func(t *testing.T) {
		_, err := auth.Exec(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'viewer',$4,ARRAY['catalog:read'],NULL)`, tenant, viewer, uuid.NewString(), c02cKeyHash(t))
		if safeCode(err) != "42501" {
			t.Fatal("C02c viewer issued an API key", safeCode(err))
		}
	})
	t.Run("unknown_scope_rejected", func(t *testing.T) {
		_, err := auth.Exec(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'scope',$4,ARRAY['unregistered:write'],NULL)`, tenant, user, uuid.NewString(), c02cKeyHash(t))
		if err == nil {
			t.Fatal("C02c unknown scope issued")
		}
	})
	t.Run("expiry_uses_database_clock", func(t *testing.T) {
		_, err := auth.Exec(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'expired',$4,ARRAY['catalog:read'],clock_timestamp()-interval '1 second')`, tenant, user, uuid.NewString(), c02cKeyHash(t))
		if err == nil {
			t.Fatal("C02c expired API key issued")
		}
	})
	t.Run("touch_valid_key", func(t *testing.T) {
		var touched bool
		if err := auth.QueryRow(ctx, `SELECT app.auth_touch_api_key($1)`, key).Scan(&touched); err != nil || !touched {
			t.Fatal("C02c active key touch refused")
		}
		var current bool
		if err := admin.QueryRow(ctx, `SELECT last_used_at IS NOT NULL AND last_used_at<=clock_timestamp() AND last_used_at>clock_timestamp()-interval '1 minute' FROM app.api_keys WHERE id=$1`, key).Scan(&current); err != nil || !current {
			t.Fatal("C02c last_used_at not set by DB clock")
		}
	})
	t.Run("other_tenant_revoke_denied", func(t *testing.T) {
		var revoked bool
		if err := auth.QueryRow(ctx, `SELECT app.auth_revoke_api_key($1,$2,$3)`, other, user, key).Scan(&revoked); err != nil || revoked {
			t.Fatal("C02c cross-tenant revoke admitted")
		}
	})
	t.Run("issuer_membership_loss", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET status='suspended' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
			t.Fatal("C02c suspension fixture failed")
		}
		var touched bool
		if err := auth.QueryRow(ctx, `SELECT app.auth_touch_api_key($1)`, key).Scan(&touched); err != nil || touched {
			t.Fatal("C02c suspended issuer key touch admitted")
		}
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
			t.Fatal("C02c fixture restore failed")
		}
	})
	t.Run("revoke_then_touch_denied", func(t *testing.T) {
		var revoked, touched bool
		if err := auth.QueryRow(ctx, `SELECT app.auth_revoke_api_key($1,$2,$3)`, tenant, user, key).Scan(&revoked); err != nil || !revoked {
			t.Fatal("C02c own-tenant revoke refused")
		}
		if err := auth.QueryRow(ctx, `SELECT app.auth_touch_api_key($1)`, key).Scan(&touched); err != nil || touched {
			t.Fatal("C02c revoked key touch admitted")
		}
	})
}

func TestC02cWriteAuthorizationPostgres(t *testing.T) {
	ctx, admin, _, _ := setupLoginPG(t)
	user := seedLoginPG(t, ctx, admin, "c02c-writer@example.test")
	var tenant, other, session string
	for _, target := range []*string{&tenant, &other} {
		if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C02c write tenant') RETURNING id::text`).Scan(target); err != nil {
			t.Fatal("C02c tenant fixture failed")
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$3,'admin'),($2,$3,'admin')`, tenant, other, user); err != nil {
		t.Fatal("C02c membership fixture failed")
	}
	if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text`, user, c02cKeyHash(t)).Scan(&session); err != nil {
		t.Fatal("C02c session fixture failed")
	}
	check := func(t *testing.T, target, kind, credential, scope string, want bool) {
		t.Helper()
		var admitted bool
		err := admin.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_runtime`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT app.auth_revalidate_tenant($1,$2,$3,$4,$5)`, target, user, kind, credential, scope).Scan(&admitted)
		})
		if err != nil || admitted != want {
			t.Fatal("C02c write-time authority mismatch", kind, scope, want, admitted, safeCode(err))
		}
	}
	t.Run("session_active", func(t *testing.T) { check(t, tenant, "session", session, "catalog:write", true) })
	t.Run("tenant_guc_binding", func(t *testing.T) { check(t, other, "session", session, "catalog:write", false) })
	t.Run("unknown_kind", func(t *testing.T) { check(t, tenant, "unknown", session, "catalog:write", false) })
	t.Run("unknown_scope", func(t *testing.T) { check(t, tenant, "session", session, "unregistered:write", false) })
	t.Run("role_downgrade", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
			t.Fatal("C02c role fixture failed")
		}
		check(t, tenant, "session", session, "catalog:write", false)
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role='admin' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
			t.Fatal("C02c role restore failed")
		}
	})
	t.Run("session_revoked", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, session); err != nil {
			t.Fatal("C02c revocation fixture failed")
		}
		check(t, tenant, "session", session, "catalog:write", false)
	})
}

// A business transaction must retain membership/session/key locks after a
// successful recheck; independent revocation and downgrade writers wait for it.
func TestC02cWriteAuthorityLocksPostgres(t *testing.T) {
	for _, kind := range []string{"session", "api_key"} {
		t.Run(kind, func(t *testing.T) {
			ctx, admin, dsn, _ := setupLoginPG(t)
			fixtureURL, err := url.Parse(dsn)
			if err != nil {
				t.Fatal("C02c lock fixture URL invalid")
			}
			adminURL, err := url.Parse(os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal("C02c admin fixture URL invalid")
			}
			fixtureURL.User = adminURL.User
			holder := c02cAuthDB(t, ctx, fixtureURL.String())
			writer := c02cAuthDB(t, ctx, dsn)
			user := seedLoginPG(t, ctx, admin, "c02c-lock@example.test")
			var tenant, credential string
			if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('C02c lock tenant') RETURNING id::text`).Scan(&tenant); err != nil {
				t.Fatal("C02c lock tenant fixture failed")
			}
			if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'admin')`, tenant, user); err != nil {
				t.Fatal("C02c lock membership fixture failed")
			}
			if kind == "session" {
				if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text`, user, c02cKeyHash(t)).Scan(&credential); err != nil {
					t.Fatal("C02c lock session fixture failed")
				}
			} else {
				credential = uuid.NewString()
				if _, err := admin.Exec(ctx, `INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,scopes) VALUES($1,$2,$3,'lock',$4,ARRAY['catalog:write'])`, tenant, credential, user, c02cKeyHash(t)); err != nil {
					t.Fatal("C02c lock key fixture failed")
				}
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				done <- holder.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
					// Use the real runtime capability in the same transaction that
					// stands for the subsequent business write.
					if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_runtime`); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
						return err
					}
					var admitted bool
					if err := tx.QueryRow(ctx, `SELECT app.auth_revalidate_tenant($1,$2,$3,$4,'catalog:write')`, tenant, user, kind, credential).Scan(&admitted); err != nil {
						return err
					}
					if !admitted {
						return errors.New("C02c valid authority refused")
					}
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("C02c runtime transaction setup failed", safeCode(err))
			case <-ctx.Done():
				t.Fatal("C02c runtime transaction setup cancelled")
			}
			var once sync.Once
			finish := func() {
				once.Do(func() {
					close(release)
					if err := <-done; err != nil {
						t.Error("C02c authority transaction failed", safeCode(err))
					}
				})
			}
			t.Cleanup(finish)
			revoked := make(chan error, 1)
			go func() {
				if kind == "session" {
					_, err := writer.Exec(ctx, `UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, credential)
					revoked <- err
				} else {
					_, err := writer.Exec(ctx, `SELECT app.auth_revoke_api_key($1,$2,$3)`, tenant, user, credential)
					revoked <- err
				}
			}()
			query := "%UPDATE app.auth_sessions SET revoked_at%"
			if kind == "api_key" {
				query = "%SELECT app.auth_revoke_api_key%"
			}
			waitLoginLocks(t, ctx, admin, query, 1)
			select {
			case err := <-revoked:
				t.Fatal("C02c revocation escaped authority lock", safeCode(err))
			default:
			}
			finish()
			if err := <-revoked; err != nil {
				t.Fatal("C02c revocation after commit failed", safeCode(err))
			}
			var valid bool
			if err := admin.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM app.`+map[string]string{"session": "auth_sessions", "api_key": "api_keys"}[kind]+` WHERE id=$1`, credential).Scan(&valid); err != nil || !valid {
				t.Fatal("C02c post-commit revocation missing")
			}
		})
	}
}

// FIX-44 exercises issuance and write-time fencing with real app_auth/runtime
// capabilities. Both tenants have active issuer memberships, so tenant denial
// cannot be explained by a missing membership or mismatched tenant GUC.
func TestFIX44ScopeAuthorityPostgres(t *testing.T) {
	ctx, admin, dsn, _ := setupLoginPG(t)
	auth := c02cAuthDB(t, ctx, dsn)
	user := seedLoginPG(t, ctx, admin, "fix44-scopes@example.test")
	var tenant, other, session string
	for _, target := range []*string{&tenant, &other} {
		if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('FIX-44 scopes') RETURNING id::text`).Scan(target); err != nil {
			t.Fatal("FIX-44 tenant fixture", safeCode(err))
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$3,'admin'),($2,$3,'admin')`, tenant, other, user); err != nil {
		t.Fatal("FIX-44 memberships", safeCode(err))
	}
	if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text`, user, c02cKeyHash(t)).Scan(&session); err != nil {
		t.Fatal("FIX-44 session", safeCode(err))
	}
	scopes := []string{"customers:privacy", "catalog:import", "catalog:export"}
	var key string
	for _, role := range []string{"owner", "admin"} {
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, tenant, user, role); err != nil {
			t.Fatal("FIX-44 issuer role", safeCode(err))
		}
		err := auth.QueryRow(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'fix44',$4,$5,NULL)::text`, tenant, user, uuid.NewString(), c02cKeyHash(t), scopes).Scan(&key)
		if err != nil {
			t.Fatal("FIX-44 approved key issuance refused", role, safeCode(err))
		}
	}
	check := func(t *testing.T, target, kind, credential, scope string, want bool) {
		t.Helper()
		var admitted bool
		err := admin.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_runtime`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, target); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT app.auth_revalidate_tenant($1,$2,$3,$4,$5)`, target, user, kind, credential, scope).Scan(&admitted)
		})
		if err != nil || admitted != want {
			t.Fatal("FIX-44 scope authority mismatch", kind, scope, want, admitted, safeCode(err))
		}
	}
	for _, role := range []string{"owner", "admin", "catalog_editor", "content_editor", "viewer"} {
		if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, tenant, user, role); err != nil {
			t.Fatal("FIX-44 role downgrade", safeCode(err))
		}
		for _, scope := range append(append([]string{}, scopes...), "unknown:scope") {
			t.Run(role+"/"+scope, func(t *testing.T) {
				want := scope != "unknown:scope" && (role == "owner" || role == "admin" || role == "catalog_editor" && scope != "customers:privacy")
				check(t, tenant, "session", session, scope, want)
				check(t, tenant, "api_key", key, scope, want)
				check(t, other, "api_key", key, scope, false)
			})
		}
	}
	if _, err := admin.Exec(ctx, `UPDATE app.tenant_memberships SET role='admin' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
		t.Fatal("FIX-44 restore role", safeCode(err))
	}
	var invalid string
	err := auth.QueryRow(ctx, `SELECT app.auth_create_api_key($1,$2,$3,'fix44',$4,ARRAY['unknown:scope'],NULL)::text`, tenant, user, uuid.NewString(), c02cKeyHash(t)).Scan(&invalid)
	if safeCode(err) != "22023" {
		t.Fatal("FIX-44 unknown scope issuance admitted", safeCode(err))
	}
}

// Go admission and transactional SQL must use the same exact role allowlists.
func TestC02cRoleScopesMirrorPostgres(t *testing.T) {
	ctx, admin, _, _ := setupLoginPG(t)
	// Discover every role admitted by the migrated membership CHECK, rather than
	// silently omitting a newly introduced role from this gate.
	var roles []string
	if err := admin.QueryRow(ctx, `SELECT array_agg(DISTINCT m[1] ORDER BY m[1])
 FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey)
 CROSS JOIN LATERAL regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''', 'g') m
 WHERE c.conrelid='app.tenant_memberships'::regclass AND c.contype='c' AND a.attname='role'`).Scan(&roles); err != nil || len(roles) == 0 {
		t.Fatal("role-scope parity: migrated role inventory unavailable", safeCode(err))
	}
	for _, role := range append(roles, "unknown", "OWNER") {
		t.Run(role, func(t *testing.T) {
			var actual []string
			if err := admin.QueryRow(ctx, `SELECT app.auth_role_scopes($1)`, role).Scan(&actual); err != nil {
				t.Fatal("C02c SQL role scopes unavailable", safeCode(err))
			}
			want := authcontract.RoleScopes(role)
			slices.Sort(actual)
			slices.Sort(want)
			if len(slices.Compact(slices.Clone(actual))) != len(actual) || !slices.Equal(actual, want) {
				t.Fatalf("ROLE_SCOPE_PARITY role=%s Go=%v DB=%v", role, want, actual)
			}
		})
	}
}

// A missing definer search path produces NULL proconfig; the actual matrix must
// reject it, roll the injected fault back, and accept the restored definition.
func TestC02cTenantMatrixRejectsNullConfigurationPostgres(t *testing.T) {
	ctx, admin, _, _ := setupLoginPG(t)
	source, err := os.ReadFile("../../../tests/db_role_matrix.sql")
	if err != nil {
		t.Fatal("C02c role matrix source unavailable")
	}
	text := string(source)
	start := strings.Index(text, "DO $tenant_auth$")
	if start < 0 {
		t.Fatal("C02c tenant role matrix block missing")
	}
	end := strings.Index(text[start:], "END $tenant_auth$;")
	if end < 0 {
		t.Fatal("C02c tenant role matrix block incomplete")
	}
	matrix := text[start : start+end+len("END $tenant_auth$;")]
	if _, err := admin.Exec(ctx, matrix); err != nil {
		t.Fatal("C02c tenant matrix rejected valid baseline", err)
	}
	err = admin.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) RESET ALL`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, matrix)
		if err == nil {
			return errors.New("C02c matrix admitted NULL function configuration")
		}
		return err
	})
	var pgErr *identityadapter.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || pgErr.Message != "TENANT_AUTH_UNSAFE_ROUTINE auth_revalidate_tenant" {
		t.Fatal("C02c matrix did not reject the injected NULL configuration", err)
	}
	if _, err := admin.Exec(ctx, matrix); err != nil {
		t.Fatal("C02c tenant matrix definition was not restored", err)
	}
}
