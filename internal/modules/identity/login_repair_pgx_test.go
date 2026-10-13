//go:build pgx

// F-28 integration proves login recovery and IPv6/cooldown admission against PG.
// It depends on the existing isolated identity fixtures and public HTTP module.
// Invariants: dead cookies clear without weakening live rotation; no cooldown waits.
package identity

import (
	"fmt"
	"net/http"
	"testing"
)

func TestLoginStaleSessionCookieRecoveryPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	for i, kind := range []string{"revoked", "idle_expired", "absolute_expired", "other_user", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			email := fmt.Sprintf("repair%d@example.test", i)
			uid := seedLoginPG(t, ctx, db, email)
			w := loginPG(t, router, email, testPassword, "198.51.100.10")
			if w.Code != 201 {
				t.Fatal("initial session refused", w.Code)
			}
			old := cookiePG(t, w, "__Host-osc_admin_session")
			var sql string
			switch kind {
			case "revoked":
				sql = "UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1"
			case "idle_expired":
				sql = "UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '2 hours',last_seen_at=clock_timestamp()-interval '31 minutes' WHERE user_id=$1"
			case "absolute_expired":
				sql = "UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '13 hours',last_seen_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1"
			case "other_user":
				other := fmt.Sprintf("repair-other%d@example.test", i)
				seedLoginPG(t, ctx, db, other)
				otherResponse := loginPG(t, router, other, testPassword, "198.51.100.11")
				if otherResponse.Code != 201 {
					t.Fatal("other session refused", otherResponse.Code)
				}
				old = cookiePG(t, otherResponse, "__Host-osc_admin_session")
			case "malformed":
				old = &http.Cookie{Name: "__Host-osc_admin_session", Value: "invalid-session-token"}
			}
			if sql != "" {
				if tag, err := db.Exec(ctx, sql, uid); err != nil || tag.RowsAffected() != 1 {
					t.Fatal("stale session fixture failed", safeCode(err))
				}
			}
			w = loginPG(t, router, email, testPassword, "198.51.100.12", old)
			if w.Code != 401 {
				t.Fatal("invalid old session did not fail conditional rotation", w.Code)
			}
			cleared := cookiePG(t, w, "__Host-osc_admin_session")
			if cleared.MaxAge != -1 || cleared.Value != "" || !cleared.Secure || !cleared.HttpOnly {
				t.Fatal("invalid session cookie not cleared")
			}
			// Model the browser applying the deletion before the next attempt.
			w = loginPG(t, router, email, testPassword, "198.51.100.13")
			if w.Code != 201 {
				t.Fatal("cookie recovery login refused", w.Code)
			}
			fresh := cookiePG(t, w, "__Host-osc_admin_session")
			if fresh.Value == old.Value || fresh.MaxAge <= 0 {
				t.Fatal("fresh session missing bounded relative lifetime")
			}
		})
	}
}

func TestLoginIPv6CountersPostgres(t *testing.T) {
	ctx, db, dsn, cfg := setupLoginPG(t)
	_, router := fixtureRouter(t, ctx, dsn, cfg)
	const email = "repair-ipv6@example.test"
	for i, ip := range []string{"2001:db8:55:66::1", "2001:db8:55:66::abcd"} {
		if i != 0 {
			for _, table := range []string{"auth_account_attempts", "auth_login_attempts"} {
				if tag, err := db.Exec(ctx, "UPDATE app."+table+" SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized=$1", email); err != nil || tag.RowsAffected() != 1 {
					t.Fatal("counter cooldown reset failed", safeCode(err))
				}
			}
		}
		w := loginPG(t, router, email, testPassword, ip)
		if w.Code != 401 {
			t.Fatal("unknown IPv6 password path refused unexpectedly", w.Code)
		}
	}
	var rows, failures int
	var host string
	if err := db.QueryRow(ctx, "SELECT count(*),sum(failure_count)::integer,min(host(ip)) FROM app.auth_login_attempts WHERE identifier_normalized=$1", email).Scan(&rows, &failures, &host); err != nil || rows != 1 || failures != 2 || host != "2001:db8:55:66::" {
		t.Fatal("IPv6 suffixes did not share durable account-IP key", rows, failures, host, safeCode(err))
	}
}
