//go:build pgx

// Responsibility: H4 password-change fence and stale-cookie recovery gates.
// Dependencies: real PG login fixtures and witnessed row locks; called by CI.
// Invariants: reset and login serialize user before session within C02c budgets.
package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"testing"
)

func TestH4FencePasswordChangePostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	const email = "fence-reset@example.test"
	uid := seedLoginPG(t, ctx, admin, email)
	for _, ip := range []string{repairInitialIP, repairOtherIP} {
		if w := loginAsSeeded(t, router, email, ip); w.Code != 201 {
			t.Fatal("reset fixture login refused", w.Code)
		}
	}
	if _, err := admin.Exec(ctx, "UPDATE app.users SET password_hash=password_hash WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1 AND revoked_at IS NULL", uid).Scan(&active); err != nil || active != 2 {
		t.Fatal("same-value password update revoked sessions", active, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE app.users SET password_hash=password_hash||'-changed' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1 AND revoked_at IS NULL", uid).Scan(&active); err != nil || active != 0 {
		t.Fatal("password update did not revoke ALL sessions", active, err)
	}
	const queued = "fence-queued@example.test"
	uid = seedLoginPG(t, ctx, admin, queued)
	client := prepareSeededLogin(t, router, queued, repairFreshIP, false)
	u, _ := url.Parse(dsn)
	w, _ := url.Parse(os.Getenv("DATABASE_URL"))
	w.Path = u.Path
	witness := c02cAuthDB(t, ctx, w.String())
	// The credential read sees the old committed PHC. The user-row wait must
	// re-read it after the writer commits; no long sleeps or broad 503 verdicts.
	release := holdLoginGate(t, ctx, admin, "UPDATE app.users SET password_hash=password_hash||'-changed' WHERE id=$1", uid)
	defer release()
	out := make(chan int, 1)
	go func() { out <- client.send().Code }()
	waitLoginLocks(t, ctx, witness, "%auth_lock_login_user%", 1)
	release()
	if code := <-out; code != 401 {
		t.Fatal("in-flight verified PHC crossed password fence", code)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&active); err != nil || active != 0 {
		t.Fatal("stale PHC created a successor", active, err)
	}
}

func TestH4FenceStaleCookieRecoveryPostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	for _, state := range []string{"malformed", "expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			email := "fence-" + state + "@example.test"
			uid := seedLoginPG(t, ctx, admin, email)
			old := "malformed"
			if state != "malformed" {
				old = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
				hash := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
				// A unique token per user is required by the global token index.
				if state == "revoked" {
					old = base64.RawURLEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))
					hash = sha256.Sum256([]byte("abcdef0123456789abcdef0123456789"))
				}
				if _, err := admin.Exec(ctx, "INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at,revoked_at) VALUES($1,$2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '2 hours',CASE WHEN $3 THEN clock_timestamp()-interval '1 hour' ELSE clock_timestamp()+interval '1 hour' END,CASE WHEN $3 THEN NULL ELSE clock_timestamp() END)", uid, hash[:], state == "expired"); err != nil {
					t.Fatal(err)
				}
			}
			if w := loginAsSeeded(t, router, email, repairFreshIP, &http.Cookie{Name: "__Host-osc_admin_session", Value: old}); w.Code != 201 {
				t.Fatal("stale cookie blocked fresh credential login", w.Code)
			}
		})
	}
}
