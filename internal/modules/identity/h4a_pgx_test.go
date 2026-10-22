//go:build pgx

// Responsibilities: witness isolated session traffic on real pooled queues.
// Dependencies: disposable login fixtures and PG lock/activity synchronization.
// Invariants: session floods cannot queue behind credential mutations; exact
// session results remain valid while optional activity encounters a busy row.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

func TestH4ARealSessionQueueIsolationPostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
	m, router := fixtureRouter(t, ctx, dsn, cfg)
	seedLoginPG(t, ctx, admin, "h4a@example.test")
	w := loginAsSeeded(t, router, "h4a@example.test", reconnectIPs[0])
	if w.Code != 201 {
		t.Fatal("initial login refused", w.Code)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookiePG(t, w, "__Host-osc_admin_session").Value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	session, err := m.repository.LookupSession(ctx, hash, time.Now(), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authURL, _ := url.Parse(dsn)
	adminURL, _ := url.Parse(os.Getenv("DATABASE_URL"))
	adminURL.Path = authURL.Path
	witness := c02cAuthDB(t, ctx, adminURL.String())
	release := holdLoginGate(t, ctx, admin, "SELECT id FROM app.auth_sessions WHERE id=$1 FOR SHARE", session.ID)
	defer release()
	done := make(chan error, 1)
	go func() { done <- m.repository.RevokeSession(ctx, session.ID, time.Now()) }()
	waitLoginLocks(t, ctx, witness, "%UPDATE app.auth_sessions SET revoked_at%", 1)
	// A second credential-lane operation really waits for the occupied pool.
	queuedCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	_, err = m.repository.Credential(queuedCtx, "h4a@example.test")
	cancel()
	if !errors.Is(err, login.ErrDatabase) {
		t.Fatal("credential queue ignored caller deadline")
	}
	// The held valid session must still be readable on the independent lane.
	results := make(chan error, 8)
	// The credential lock stays held until release() below, so a lookup queued behind it would
	// still time out; the window only has to absorb slow runners serialising 8 lookups on one lane
	// (200ms flaked on main run 39789408706).
	floodCtx, cancelFlood := context.WithTimeout(ctx, 3*time.Second)
	defer cancelFlood()
	for i := 0; i < cap(results); i++ {
		go func() {
			s, e := m.repository.LookupSession(floodCtx, hash, time.Now(), 30*time.Minute)
			if e == nil && s.ID != session.ID {
				e = login.ErrSession
			}
			results <- e
		}()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Error("session flood queued behind credential lane", err)
		}
	}
	var blocked int
	if err := witness.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%UPDATE app.auth_sessions SET revoked_at%'").Scan(&blocked); err != nil || blocked != 1 {
		t.Error("missing live credential lock witness")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal("credential mutation failed after release", err)
	}
}
