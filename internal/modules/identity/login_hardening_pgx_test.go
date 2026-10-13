//go:build pgx

// Responsibility: H-4 DB-clock, lock-expiry and password-reset regressions.
// Dependencies: isolated PG fixtures, seeded HTTP clients and private sqlc store.
// Callers: CI pgx gate; invariants: lock barriers are witnessed before release,
// host skew cannot grant admission, reset cannot leave or recreate live sessions.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/store"
	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func hardeningWitness(t *testing.T, ctx context.Context, dsn string) *testLoginDB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("fixture URL invalid")
	}
	admin, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal("admin fixture URL invalid")
	}
	u.User = admin.User
	runner, err := identityadapter.OpenIdentity(ctx, u.String())
	if err != nil {
		t.Fatal("clock witness unavailable")
	}
	t.Cleanup(func() { _ = runner.Close(context.Background()) })
	return &testLoginDB{runner: runner}
}

func hardeningRepository(t *testing.T, ctx context.Context, dsn string) *store.LoginRepository {
	t.Helper()
	r, err := store.OpenLogin(ctx, dsn, 30*time.Minute)
	if err != nil {
		t.Fatal("repository fixture refused")
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func hardeningHash(t *testing.T, encoded string) [32]byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		t.Fatal("fixture token invalid")
	}
	return sha256.Sum256(raw)
}

func hardeningEntropy(t *testing.T) [32]byte {
	t.Helper()
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal("fixture entropy unavailable")
	}
	return value
}

// Poll the database clock; scheduling or KDF throughput cannot stand in for expiry.
func hardeningWaitUntil(t *testing.T, ctx context.Context, witness *testLoginDB, until time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var expired bool
		if err := witness.QueryRow(ctx, "SELECT clock_timestamp()>$1", until).Scan(&expired); err != nil {
			t.Fatal("database clock witness failed")
		}
		if expired {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("database clock witness cancelled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("database clock witness timed out")
}

func TestH4DatabaseClockAdmissionPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	for _, delta := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			cfg.Now = func() time.Time { return time.Now().Add(delta) }
			_, router := fixtureRouter(t, ctx, dsn, cfg)
			email := "clock" + delta.String() + "@example.test"
			uid := seedLoginPG(t, ctx, db, email)
			if _, err := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES($1,10,clock_timestamp()+interval '60 seconds')", email); err != nil {
				t.Fatal("active cooldown fixture failed")
			}
			if w := loginAsSeeded(t, router, email, repairInitialIP); w.Code != 429 {
				t.Fatal("host skew bypassed active database cooldown", w.Code)
			}
			if _, err := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized=$1", email); err != nil {
				t.Fatal("expired cooldown fixture failed")
			}
			w := loginAsSeeded(t, router, email, repairFreshIP)
			if w.Code != 201 {
				t.Fatal("host skew preserved expired database cooldown", w.Code)
			}
			c := cookiePG(t, w, "__Host-osc_admin_session")
			if got := requestPG(router, "GET", "/api/v1/auth/session", "", "", repairFreshIP, c); got.Code != 200 {
				t.Fatal("host skew rejected database-valid session", got.Code)
			}
			var clocksValid bool
			if err := db.QueryRow(ctx, "SELECT bool_and(created_at<=clock_timestamp() AND last_seen_at<=clock_timestamp() AND expires_at>clock_timestamp()) FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&clocksValid); err != nil || !clocksValid {
				t.Fatal("session clocks derived from skewed host")
			}
		})
	}
}

func TestH4ExpiredDeviceHasNoExemptionPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	const email = "expired-device-h4@example.test"
	uid := seedLoginPG(t, ctx, db, email)
	w := loginAsSeeded(t, router, email, deviceInitialIP)
	if w.Code != 201 {
		t.Fatal("device fixture login refused", w.Code)
	}
	c := cookiePG(t, w, "__Host-osc_known_device")
	// Replace only this disposable fixture; do not weaken the immutable-clock guard.
	payload, _, _ := strings.Cut(c.Value, ".")
	hash := hardeningHash(t, payload)
	if _, err := db.Exec(ctx, "DELETE FROM app.known_devices WHERE user_id=$1", uid); err != nil {
		t.Fatal("device fixture replacement failed")
	}
	if _, err := db.Exec(ctx, "INSERT INTO app.known_devices(user_id,cookie_hash,created_at,expires_at) VALUES($1,$2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')", uid, hash[:]); err != nil {
		t.Fatal("expired device fixture failed")
	}
	if _, err := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES($1,10,clock_timestamp()+interval '60 seconds')", email); err != nil {
		t.Fatal("device cooldown fixture failed")
	}
	cfg.Now = func() time.Time { return time.Now().Add(-24 * time.Hour) }
	_, skewed := fixtureRouter(t, ctx, dsn, cfg)
	if got := loginAsSeeded(t, skewed, email, deviceRecoveryIP, c); got.Code != 429 {
		t.Fatal("expired device retained account exemption under host skew", got.Code)
	}
	if _, err := hardeningRepository(t, ctx, dsn).Device(ctx, hash); !errors.Is(err, login.ErrNotFound) {
		t.Fatal("expired device returned by repository")
	}
}

func TestH4LockedSessionExpiryPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	witness := hardeningWitness(t, ctx, dsn)
	for _, operation := range []string{"lookup", "rotation"} {
		t.Run(operation, func(t *testing.T) {
			email := "locked-" + operation + "@example.test"
			uid := seedLoginPG(t, ctx, db, email)
			w := loginAsSeeded(t, router, email, rotationInitialIP)
			if w.Code != 201 {
				t.Fatal("session fixture login refused", w.Code)
			}
			hash := hardeningHash(t, cookiePG(t, w, "__Host-osc_admin_session").Value)
			var until time.Time
			if err := db.QueryRow(ctx, "UPDATE app.auth_sessions SET expires_at=clock_timestamp()+interval '2 seconds' WHERE token_hash=$1 RETURNING expires_at", hash[:]).Scan(&until); err != nil {
				t.Fatal("short expiry fixture failed")
			}
			r := hardeningRepository(t, ctx, dsn)
			release := holdLoginGate(t, ctx, db, "SELECT id FROM app.auth_sessions WHERE token_hash=$1 FOR UPDATE", hash[:])
			defer release()
			out := make(chan error, 1)
			if operation == "lookup" {
				go func() { _, err := r.LookupSession(ctx, hash, time.Now(), 30*time.Minute); out <- err }()
			} else {
				at := time.Now()
				in := login.SessionInput{UserID: uid, VerifiedPasswordHash: seededLoginPHC(), TokenHash: hardeningEntropy(t), DeviceHash: hardeningEntropy(t), CreatedAt: at, ExpiresAt: at.Add(time.Hour), DeviceExpiresAt: at.Add(24 * time.Hour), OldTokenHash: &hash}
				go func() { _, err := r.SaveLogin(ctx, in); out <- err }()
			}
			waitLoginLocks(t, ctx, witness, "%app.auth_sessions%", 1)
			hardeningWaitUntil(t, ctx, witness, until)
			release()
			if err := <-out; !errors.Is(err, login.ErrSession) {
				t.Fatal("unchanged locked row admitted after database expiry", operation)
			}
			var count int
			if err := db.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&count); err != nil || count != 1 {
				t.Fatal("expired rotation inserted successor", count)
			}
		})
	}
}

func TestH4FailureClockAfterAdvisoryWaitPostgres(t *testing.T) {
	ctx, db, dsn, _ := setupLoginPG(t)
	witness := hardeningWitness(t, ctx, dsn)
	r := hardeningRepository(t, ctx, dsn)
	const email = "queued-failure-h4@example.test"
	release := holdLoginGate(t, ctx, db, "SELECT pg_advisory_xact_lock(hashtextextended('opensite:auth:'||$1,0))", email)
	defer release()
	out := make(chan error, 1)
	go func() { out <- r.RecordFailure(ctx, email, netip.MustParseAddr(concurrentFailureIP)) }()
	waitLoginLocks(t, ctx, witness, "%auth_record_failure%", 1)
	var until time.Time
	if err := witness.QueryRow(ctx, "SELECT clock_timestamp()+interval '2 seconds'").Scan(&until); err != nil {
		t.Fatal("failure clock barrier unavailable")
	}
	hardeningWaitUntil(t, ctx, witness, until)
	release()
	if err := <-out; err != nil {
		t.Fatal("failure bookkeeping refused")
	}
	var live bool
	if err := db.QueryRow(ctx, "SELECT cooldown_until>clock_timestamp() FROM app.auth_account_attempts WHERE identifier_normalized=$1", email).Scan(&live); err != nil || !live {
		t.Fatal("advisory wait consumed durable failure cooldown")
	}
}

func TestH4PasswordChangeRevocationPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	cfg.HashTimeout = 20 * time.Second
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	witness := hardeningWitness(t, ctx, dsn)
	t.Run("all_sessions_revoked", func(t *testing.T) {
		const email = "reset-h4@example.test"
		uid := seedLoginPG(t, ctx, db, email)
		for _, ip := range []string{repairInitialIP, repairOtherIP} {
			if w := loginAsSeeded(t, router, email, ip); w.Code != 201 {
				t.Fatal("reset fixture login refused", w.Code)
			}
		}
		if _, err := db.Exec(ctx, "UPDATE app.users SET password_hash=password_hash WHERE id=$1", uid); err != nil {
			t.Fatal("unchanged hash fixture failed")
		}
		var unchanged int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1 AND revoked_at IS NULL", uid).Scan(&unchanged); err != nil || unchanged != 2 {
			t.Fatal("unchanged password revoked sessions", unchanged)
		}
		if _, err := db.Exec(ctx, "UPDATE app.users SET password_hash=password_hash||'-changed' WHERE id=$1", uid); err != nil {
			t.Fatal("password reset fixture failed")
		}
		var active int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1 AND revoked_at IS NULL", uid).Scan(&active); err != nil || active != 0 {
			t.Fatal("password update did not revoke all sessions", active)
		}
	})
	t.Run("inflight_old_hash_rejected", func(t *testing.T) {
		const email = "concurrent-reset-h4@example.test"
		uid := seedLoginPG(t, ctx, db, email)
		// Stage an uncommitted password change. The login sees the old committed
		// PHC but must recheck it after waiting for the user's row lock.
		client := prepareSeededLogin(t, router, email, repairFreshIP, false)
		release := holdLoginGate(t, ctx, db, "UPDATE app.users SET password_hash=password_hash||'-changed-again' WHERE id=$1", uid)
		defer release()
		out := make(chan int, 1)
		go func() { out <- client.send().Code }()
		waitLoginLocks(t, ctx, witness, "%app.users%", 1)
		release()
		if code := <-out; code != 401 {
			t.Fatal("in-flight old credential recreated session after reset", code)
		}
		var active int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1 AND revoked_at IS NULL", uid).Scan(&active); err != nil || active != 0 {
			t.Fatal("reset left concurrent successor active", active)
		}
	})
}

func TestH4UserLockFencePostgres(t *testing.T) {
	ctx, db, dsn, _ := setupLoginPG(t)
	const email = "user-lock-h4@example.test"
	uid := seedLoginPG(t, ctx, db, email)
	runner, err := identityadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		t.Fatal("user lock fixture unavailable")
	}
	defer runner.Close(context.Background())
	auth := &testLoginDB{runner: runner}
	for _, probe := range []struct {
		name, hash string
		allowed    bool
	}{
		{"verified_hash", seededLoginPHC(), true},
		{"stale_hash", seededLoginPHC() + "-stale", false},
		{"empty_hash", "", false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			var allowed bool
			if err := auth.QueryRow(ctx, "SELECT app.auth_lock_login_user($1,$2)", uid, probe.hash).Scan(&allowed); err != nil || allowed != probe.allowed {
				t.Fatal("restricted user lock returned incorrect verdict", safeCode(err))
			}
		})
	}
	if _, err := db.Exec(ctx, "UPDATE app.users SET status='disabled' WHERE id=$1", uid); err != nil {
		t.Fatal("disabled lock fixture failed")
	}
	var allowed bool
	if err := auth.QueryRow(ctx, "SELECT app.auth_lock_login_user($1,$2)", uid, seededLoginPHC()).Scan(&allowed); err != nil || allowed {
		t.Fatal("disabled user passed restricted lock")
	}
}
