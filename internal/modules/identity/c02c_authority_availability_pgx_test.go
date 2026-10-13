//go:build pgx

// Responsibilities: witness bounded staff authority under long business writes.
// Dependencies: disposable login PG fixtures and the existing tenant public ports.
// Invariants: FOR SHARE protects revocation; optional activity cannot queue auth;
// a blocked key mutation cannot monopolize unrelated staff membership lookups.
package identity

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	authcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant"
	keycontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
)

func TestC02cAuthorityAvailabilityPostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
	module, err := tenant.Open(ctx, tenant.Config{DatabaseURL: dsn})
	if err != nil {
		t.Fatal("authority fixture unavailable")
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })
	authority := module.Authorization()
	management, ok := authority.(keycontract.Repository)
	if !ok {
		t.Fatal("existing tenant management port unavailable")
	}
	loginModule, err := OpenLogin(ctx, dsn, cfg)
	if err != nil {
		t.Fatal("session fixture unavailable")
	}
	t.Cleanup(func() { _ = loginModule.Close(context.Background()) })
	authURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("authority fixture configuration invalid")
	}
	adminURL, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal("witness fixture configuration invalid")
	}
	adminURL.Path = authURL.Path
	witness := c02cAuthDB(t, ctx, adminURL.String())
	issuer := seedLoginPG(t, ctx, admin, "availability-issuer@example.test")
	other := seedLoginPG(t, ctx, admin, "availability-other@example.test")
	var tenantID string
	if err := admin.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('Authority availability') RETURNING id::text`).Scan(&tenantID); err != nil {
		t.Fatal("tenant fixture failed")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'admin')`, tenantID, issuer, other); err != nil {
		t.Fatal("membership fixture failed")
	}
	createKey := func(t *testing.T) string {
		t.Helper()
		id := uuid.NewString()
		var hash [32]byte
		copy(hash[:], c02cKeyHash(t))
		if _, err := management.Create(ctx, tenantID, issuer, keycontract.KeyMaterial{ID: id, Prefix: id[:8], Hash: hash, Scopes: []string{"catalog:read"}}); err != nil {
			t.Fatal("key fixture failed")
		}
		return id
	}
	keyID := createKey(t)
	var sessionID string
	var sessionHash [32]byte
	copy(sessionHash[:], c02cKeyHash(t))
	if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,clock_timestamp()-interval '3 minutes',clock_timestamp()-interval '2 minutes',clock_timestamp()+interval '1 hour') RETURNING id::text`, issuer, sessionHash[:]).Scan(&sessionID); err != nil {
		t.Fatal("session fixture failed", safeCode(err))
	}

	t.Run("busy_key_activity_does_not_wait", func(t *testing.T) {
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, keyID)
		defer release()
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := authority.TouchKey(callCtx, keyID); err != nil {
			t.Fatal("valid key activity blocked behind business authority lock")
		}
		if member, err := authority.Membership(callCtx, other, tenantID); err != nil || member.UserID != other {
			t.Fatal("unrelated staff membership unavailable during key activity")
		}
		var unchanged bool
		if err := witness.QueryRow(ctx, `SELECT last_used_at IS NULL FROM app.api_keys WHERE id=$1`, keyID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("busy key activity modified the held row")
		}
	})

	t.Run("busy_session_activity_does_not_wait", func(t *testing.T) {
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.auth_sessions WHERE id=$1 FOR SHARE`, sessionID)
		defer release()
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		session, err := loginModule.repository.LookupSession(callCtx, sessionHash, time.Now(), 30*time.Minute)
		if err != nil || session.ID != sessionID {
			t.Fatal("valid session activity blocked behind business authority lock")
		}
		if member, err := authority.Membership(callCtx, other, tenantID); err != nil || member.UserID != other {
			t.Fatal("unrelated staff membership unavailable during session activity")
		}
		var unchanged bool
		if err := witness.QueryRow(ctx, `SELECT last_seen_at<clock_timestamp()-interval '1 minute' FROM app.auth_sessions WHERE id=$1`, sessionID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("busy session activity modified the held row")
		}
	})

	t.Run("blocked_revoke_does_not_own_all_authority_connections", func(t *testing.T) {
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, keyID)
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		finished := make(chan struct{})
		go func() {
			_ = management.Revoke(callCtx, tenantID, issuer, keyID)
			close(finished)
		}()
		defer func() { release(); <-finished }()
		waitLoginLocks(t, callCtx, witness, "%auth_revoke_api_key%", 1)
		lookupCtx, lookupCancel := context.WithTimeout(ctx, time.Second)
		defer lookupCancel()
		member, err := authority.Membership(lookupCtx, other, tenantID)
		if err != nil || member.UserID != other {
			t.Fatal("blocked revoke starved unrelated staff membership")
		}
		select {
		case <-finished:
			t.Fatal("membership only completed after the blocked revoke released its connection")
		default:
		}
	})

	t.Run("mandatory_lock_wait_is_bounded_without_caller_deadline", func(t *testing.T) {
		// The long parent fixture deadline is intentionally longer than the
		// production operation bound; request callers need not supply a deadline.
		boundedKey := createKey(t)
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, boundedKey)
		defer release()
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- management.Revoke(callCtx, tenantID, issuer, boundedKey) }()
		select {
		case err := <-finished:
			if !errors.Is(err, authcontract.ErrUnavailable) {
				t.Fatal("blocked authority mutation did not return sanitized unavailable")
			}
		case <-time.After(2500 * time.Millisecond):
			cancel()
			<-finished
			t.Fatal("authority lock wait exceeded the internal two-second operation bound")
		}
	})

	t.Run("staff_session_mutation_has_internal_bound_and_recovers", func(t *testing.T) {
		var independentHash [32]byte
		copy(independentHash[:], c02cKeyHash(t))
		var independentID string
		if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text`, other, independentHash[:]).Scan(&independentID); err != nil {
			t.Fatal("independent session fixture failed", safeCode(err))
		}
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.auth_sessions WHERE id=$1 FOR SHARE`, sessionID)
		defer release()
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- loginModule.repository.RevokeSession(callCtx, sessionID, time.Now()) }()
		select {
		case err := <-finished:
			if !errors.Is(err, login.ErrDatabase) {
				t.Fatal("blocked staff session mutation did not return sanitized database unavailable")
			}
		case <-time.After(2500 * time.Millisecond):
			cancel()
			<-finished
			t.Fatal("staff session lock wait exceeded the internal two-second operation bound")
		}
		release()
		lookupCtx, lookupCancel := context.WithTimeout(ctx, time.Second)
		defer lookupCancel()
		session, err := loginModule.repository.LookupSession(lookupCtx, independentHash, time.Now(), 30*time.Minute)
		if err != nil || session.ID != independentID {
			t.Fatal("staff authentication did not recover after bounded lock failure")
		}
	})
}
