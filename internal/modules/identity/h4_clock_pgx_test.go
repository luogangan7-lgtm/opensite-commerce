//go:build pgx

// Responsibility: clock-only H4 regression gates on disposable PostgreSQL.
// Dependencies: seeded login HTTP clients and private repository fixtures.
// Invariants: host skew never controls durable creation, admission or revocation.
package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

func TestH4ClockAdmissionPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	for _, delta := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			cfg.Now = func() time.Time { return time.Now().Add(delta) }
			m, router := fixtureRouter(t, ctx, dsn, cfg)
			email := "clock" + delta.String() + "@example.test"
			uid := seedLoginPG(t, ctx, db, email)
			if _, err := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES($1,10,clock_timestamp()+interval '60 seconds')", email); err != nil {
				t.Fatal(err)
			}
			if w := loginAsSeeded(t, router, email, repairInitialIP); w.Code != 429 {
				t.Fatal("host skew bypassed DB cooldown", w.Code)
			}
			if _, err := db.Exec(ctx, "UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized=$1", email); err != nil {
				t.Fatal(err)
			}
			w := loginAsSeeded(t, router, email, repairFreshIP)
			if w.Code != 201 {
				t.Fatal("expired DB cooldown blocked login", w.Code)
			}
			cookie := cookiePG(t, w, "__Host-osc_admin_session")
			if got := requestPG(router, "GET", "/api/v1/auth/session", "", "", repairFreshIP, cookie); got.Code != 200 {
				t.Fatal("host skew rejected DB-valid session", got.Code)
			}
			var valid bool
			if err := db.QueryRow(ctx, "SELECT s.created_at BETWEEN clock_timestamp()-interval '30 seconds' AND clock_timestamp() AND s.last_seen_at>=s.created_at AND s.expires_at-s.created_at=interval '12 hours' AND d.created_at BETWEEN clock_timestamp()-interval '30 seconds' AND clock_timestamp() AND d.expires_at-d.created_at=interval '90 days' FROM app.auth_sessions s JOIN app.known_devices d USING(user_id) WHERE s.user_id=$1", uid).Scan(&valid); err != nil || !valid {
				t.Fatal("creation/lifetimes derived from host clock", err)
			}
			raw, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
			hash := sha256.Sum256(raw)
			session, err := m.repository.LookupSession(ctx, hash, cfg.Now(), 30*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.repository.RevokeSession(ctx, session.ID, cfg.Now()); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, "SELECT revoked_at BETWEEN clock_timestamp()-interval '30 seconds' AND clock_timestamp() FROM app.auth_sessions WHERE id=$1", session.ID).Scan(&valid); err != nil || !valid {
				t.Fatal("logout revocation used host time", err)
			}
			if got := requestPG(router, "GET", "/api/v1/auth/session", "", "", repairFreshIP, cookie); got.Code != 401 {
				t.Fatal("revoked session outcome", got.Code)
			}
		})
	}
}

func TestH4ClockExpiredAuthorityPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	m, router := fixtureRouter(t, ctx, dsn, cfg)
	const email = "expired-clock@example.test"
	uid := seedLoginPG(t, ctx, db, email)
	w := loginAsSeeded(t, router, email, deviceInitialIP)
	if w.Code != 201 {
		t.Fatal("device fixture refused", w.Code)
	}
	cookie := cookiePG(t, w, "__Host-osc_known_device")
	payload, _, _ := strings.Cut(cookie.Value, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	hash := sha256.Sum256(raw)
	if _, err := db.Exec(ctx, "DELETE FROM app.known_devices WHERE user_id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO app.known_devices(user_id,cookie_hash,created_at,expires_at) VALUES($1,$2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')", uid, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := m.repository.Device(ctx, hash); !errors.Is(err, login.ErrNotFound) {
		t.Fatal("expired device returned as authority", err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO app.auth_account_attempts(identifier_normalized,failure_count,cooldown_until) VALUES($1,10,clock_timestamp()+interval '60 seconds')", email); err != nil {
		t.Fatal(err)
	}
	cfg.Now = func() time.Time { return time.Now().Add(-24 * time.Hour) }
	_, skewed := fixtureRouter(t, ctx, dsn, cfg)
	if got := loginAsSeeded(t, skewed, email, deviceRecoveryIP, cookie); got.Code != 429 {
		t.Fatal("expired device kept cooldown exemption", got.Code)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	expiredHash := sha256.Sum256(make([]byte, 32))
	if _, err := db.Exec(ctx, "INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')", uid, expiredHash[:]); err != nil {
		t.Fatal(err)
	}
	if got := requestPG(skewed, "GET", "/api/v1/auth/session", "", "", repairFreshIP, &http.Cookie{Name: "__Host-osc_admin_session", Value: token}); got.Code != 401 {
		t.Fatal("expired session outcome", got.Code)
	}
}
