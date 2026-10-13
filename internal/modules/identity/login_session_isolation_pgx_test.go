//go:build pgx

// Responsibility: prove blocked session lookup traffic cannot occupy the login lane.
// Dependencies: isolated PG fixtures, identity private repository and public login HTTP.
// Invariants: a witnessed row-lock wait remains active while owner login commits;
// lookup deadlines, cleanup and result channels bound every concurrent operation.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	identityadapter "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestSessionFloodCannotStarveOwnerLoginPostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
	module, router := fixtureRouter(t, ctx, dsn, cfg)
	const email = "session-lane-owner@example.test"
	uid := seedLoginPG(t, ctx, admin, email)
	w := loginAsSeeded(t, router, email, reconnectIPs[0])
	if w.Code != http.StatusCreated {
		t.Fatal("initial owner login refused", w.Code)
	}
	sessionCookie := cookiePG(t, w, "__Host-osc_admin_session")
	deviceCookie := cookiePG(t, w, "__Host-osc_known_device")
	raw, err := base64.RawURLEncoding.DecodeString(sessionCookie.Value)
	if err != nil {
		t.Fatal("fixture session token invalid")
	}
	hash := sha256.Sum256(raw)
	owner := prepareSeededLogin(t, router, email, reconnectIPs[1], false, deviceCookie)

	// The lock holder owns admin's only connection, so observe it through a
	// second admin connection rather than an extra auth connection or pool.
	adminURL, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal("fixture admin DSN invalid")
	}
	authURL, err := url.Parse(dsn)
	if err != nil || authURL.User == nil {
		t.Fatal("fixture auth DSN invalid")
	}
	adminURL.Path = authURL.Path
	witnessRunner, err := identityadapter.OpenIdentity(ctx, adminURL.String())
	if err != nil {
		t.Fatal("fixture lock observer unavailable")
	}
	defer witnessRunner.Close(context.Background())
	witness := &testLoginDB{runner: witnessRunner}
	release := holdLoginGate(t, ctx, admin, "SELECT id FROM app.auth_sessions WHERE token_hash=$1 FOR UPDATE", hash[:])
	defer release()

	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	const flood = 16
	results := make(chan error, flood+1)
	lookup := func() {
		_, err := module.repository.LookupSession(lookupCtx, hash, time.Now(), 30*time.Minute)
		results <- err
	}
	go lookup()
	// Matches both the baseline UPDATE lookup and the hardened explicit lock
	// query: only auth_sessions business SQL blocked by our holder is counted.
	waitLoginLocks(t, lookupCtx, witness, "%app.auth_sessions%", 1)
	started := make(chan struct{}, flood)
	for i := 0; i < flood; i++ {
		go func() {
			started <- struct{}{}
			lookup()
		}()
	}
	for i := 0; i < flood; i++ {
		select {
		case <-started:
		case <-lookupCtx.Done():
			t.Fatal("session flood startup deadline exceeded")
		}
	}

	ownerResult := make(chan int, 1)
	go func() { ownerResult <- owner.send().Code }()
	select {
	case status := <-ownerResult:
		if status != http.StatusCreated {
			t.Fatal("blocked session lane starved owner login", status)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("owner login deadline exceeded while session lane was blocked")
	}
	var blocked, sessions int
	if err = witness.QueryRow(ctx, "SELECT count(*)::integer FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND query LIKE '%app.auth_sessions%'", authURL.User.Username()).Scan(&blocked); err != nil || blocked != 1 {
		t.Fatal("owner login passed without a live isolated session lock witness", blocked, safeCode(err))
	}
	if err = witness.QueryRow(ctx, "SELECT count(*)::integer FROM app.auth_sessions WHERE user_id=$1", uid).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatal("isolated owner login did not commit exactly one new session", sessions, safeCode(err))
	}
	release()
	for i := 0; i < flood+1; i++ {
		select {
		case err = <-results:
			if err != nil {
				t.Fatal("session lookup did not recover after releasing its lane", safeCode(err))
			}
		case <-lookupCtx.Done():
			t.Fatal("session lookup drain deadline exceeded")
		}
	}
}
