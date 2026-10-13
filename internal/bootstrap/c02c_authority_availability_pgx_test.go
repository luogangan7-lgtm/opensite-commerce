//go:build pgx

// Responsibilities: witness independent tenant authority during held key writes.
// Dependencies: existing bootstrap PG fixture and public tenant/identity ports.
// Invariants: FOR SHARE preserves revocation atomicity, optional activity skips
// busy credentials, and unrelated staff authority has bounded independent lanes.
package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	authcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant"
	keycontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestC02cTenantAuthorityAvailabilityPostgres(t *testing.T) {
	ctx, admin, fixtureDSN, authDSN, _ := c02cHTTPPGFixture(t)
	module, err := tenant.Open(ctx, tenant.Config{DatabaseURL: authDSN})
	if err != nil {
		t.Fatal("authority fixture unavailable")
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })
	authority := module.Authorization()
	management, ok := authority.(keycontract.Repository)
	if !ok {
		t.Fatal("existing tenant management port unavailable")
	}
	witness, err := pgxadapter.OpenIdentity(ctx, fixtureDSN)
	if err != nil {
		t.Fatal("authority lock witness unavailable")
	}
	t.Cleanup(func() { _ = witness.Close(context.Background()) })
	readWitness := func(sql string, destination any, args ...any) error {
		return witness.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(destination)
		})
	}
	waitRevokeLock := func(t *testing.T, waitCtx context.Context) {
		t.Helper()
		for {
			var waiting int
			if err := readWitness(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%auth_revoke_api_key%'`, &waiting); err != nil {
				t.Fatal("lock witness unavailable")
			}
			if waiting >= 1 {
				return
			}
			select {
			case <-waitCtx.Done():
				t.Fatal("production revoke never reached witnessed lock")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	var issuer, other, tenantID string
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i, out := range []*string{&issuer, &other} {
			phc := "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(c02cHTTPRandom(t, 16)) + "$" + base64.RawStdEncoding.EncodeToString(c02cHTTPRandom(t, 32))
			if err := tx.QueryRow(ctx, `INSERT INTO app.users(email,password_hash) VALUES($1,$2) RETURNING id::text`, []string{"availability-issuer@example.test", "availability-other@example.test"}[i], phc).Scan(out); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO app.tenants(name) VALUES('Authority availability') RETURNING id::text`).Scan(&tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'admin')`, tenantID, issuer, other)
		return err
	}); err != nil {
		t.Fatal("authority seed fixture failed")
	}
	createKey := func(t *testing.T) string {
		t.Helper()
		id := uuid.NewString()
		hash := sha256.Sum256(c02cHTTPRandom(t, 32))
		if _, err := management.Create(ctx, tenantID, issuer, keycontract.KeyMaterial{ID: id, Prefix: id[:8], Hash: hash, Scopes: []string{"catalog:read"}}); err != nil {
			t.Fatal("key fixture failed")
		}
		return id
	}
	keyID := createKey(t)
	t.Run("busy_key_activity_does_not_wait", func(t *testing.T) {
		release := c02cHoldAuthorityRow(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, keyID)
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
		if err := readWitness(`SELECT last_used_at IS NULL FROM app.api_keys WHERE id=$1`, &unchanged, keyID); err != nil || !unchanged {
			t.Fatal("busy key activity modified the held row")
		}
	})

	t.Run("blocked_revoke_does_not_own_all_authority_connections", func(t *testing.T) {
		release := c02cHoldAuthorityRow(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, keyID)
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		finished := make(chan struct{})
		go func() {
			_ = management.Revoke(callCtx, tenantID, issuer, keyID)
			close(finished)
		}()
		defer func() { release(); <-finished }()
		waitRevokeLock(t, callCtx)
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
		release := c02cHoldAuthorityRow(t, ctx, admin, `SELECT id FROM app.api_keys WHERE id=$1 FOR SHARE`, boundedKey)
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

}

// Hold only the fixture transaction; a separate physical session observes waits.
// No connection, transaction or pgx row escapes the adapter callback.
func c02cHoldAuthorityRow(t *testing.T, ctx context.Context, admin *pgxadapter.IdentityRunner, sql string, args ...any) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-locked:
	case <-done:
		t.Fatal("authority gate lock failed")
	case <-ctx.Done():
		t.Fatal("authority gate lock cancelled")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Fatal("authority gate release failed")
			}
		})
	}
}
