//go:build pgx

// Responsibility: prove reconnects reject changed auth authority before login work.
// Dependencies: disposable PG fixtures, existing seeded clients and public identity HTTP.
// Invariants: invalid role/session defaults produce redacted 503 and no new session;
// restoring authority admits a fresh physical connection without reopening the module.
package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestLoginRejectsChangedAuthorityAfterReconnectPostgres(t *testing.T) {
	for _, scenario := range []struct {
		name, change, restore string
	}{
		{"role_authority", " CREATEROLE", " NOCREATEROLE"},
		{"session_role", " SET role TO 'app_auth'", " RESET role"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, admin, dsn, cfg := setupLoginPG(t)
			_, router := fixtureRouter(t, ctx, dsn, cfg)
			const email = "reconnect-admission@example.test"
			uid := seedLoginPG(t, ctx, admin, email)
			if w := loginAsSeeded(t, router, email, reconnectIPs[0]); w.Code != http.StatusCreated {
				t.Fatal("initial login refused", w.Code)
			}
			parsed, err := url.Parse(dsn)
			if err != nil || parsed.User == nil {
				t.Fatal("fixture DSN invalid")
			}
			role := parsed.User.Username()
			alter := "ALTER ROLE " + pgx.Identifier{role}.Sanitize()
			if fixturePSQL(ctx, os.Getenv("DATABASE_URL"), alter+scenario.change+";") != nil {
				t.Fatal("auth authority change fixture failed")
			}
			// Role defaults live outside the database, so restoration must precede
			// setupLoginPG's role cleanup even when the regression fails.
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if fixturePSQL(cleanupCtx, os.Getenv("DATABASE_URL"), alter+scenario.restore+";") != nil {
					t.Error("auth authority restoration failed")
				}
			})
			terminateAuthBackends(t, ctx, admin, role)
			w := loginAsSeeded(t, router, email, reconnectIPs[1])
			if w.Code != http.StatusServiceUnavailable {
				t.Fatal("changed authority reached login work after reconnect", w.Code)
			}
			var problem struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Code != "auth_unavailable" {
				t.Fatal("changed authority did not return sanitized auth problem")
			}
			for _, private := range []string{role, dsn, "CREATEROLE", "current_user", "SQLSTATE"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("auth problem exposed connection or admission details")
				}
			}
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == "__Host-osc_admin_session" && cookie.Value != "" {
					t.Fatal("failed admission issued a session cookie")
				}
			}
			var sessions int
			if err = admin.QueryRow(ctx, "SELECT count(*)::integer FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&sessions); err != nil || sessions != 1 {
				t.Fatal("failed admission inserted a session", sessions, safeCode(err))
			}
			if fixturePSQL(ctx, os.Getenv("DATABASE_URL"), alter+scenario.restore+";") != nil {
				t.Fatal("auth authority restore fixture failed")
			}
			terminateAuthBackends(t, ctx, admin, role)
			if w = loginAsSeeded(t, router, email, reconnectIPs[0]); w.Code != http.StatusCreated {
				t.Fatal("restored authority did not recover login", w.Code)
			}
			if err = admin.QueryRow(ctx, "SELECT count(*)::integer FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&sessions); err != nil || sessions != 2 {
				t.Fatal("restored authority did not insert exactly one session", sessions, safeCode(err))
			}
		})
	}
}

func terminateAuthBackends(t *testing.T, parent context.Context, admin *testLoginDB, role string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var killed int
	if err := admin.QueryRow(ctx, "SELECT count(pg_terminate_backend(pid))::integer FROM pg_stat_activity WHERE usename=$1", role).Scan(&killed); err != nil || killed < 1 {
		t.Fatal("auth backend termination fixture failed", killed, safeCode(err))
	}
	for {
		var alive int
		if err := admin.QueryRow(ctx, "SELECT count(*)::integer FROM pg_stat_activity WHERE usename=$1", role).Scan(&alive); err != nil {
			t.Fatal("auth backend observation failed", safeCode(err))
		}
		if alive == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("auth backend termination deadline exceeded")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
