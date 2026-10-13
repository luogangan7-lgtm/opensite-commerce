//go:build pgx

// Responsibilities: witness bounded staff authority under long business writes.
// Dependencies: disposable login PG fixtures and the private staff repository.
// Invariants: FOR SHARE protects revocation; optional activity cannot queue auth;
// a failed mandatory session mutation releases its serialized connection.
package identity

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

func TestC02cAuthorityAvailabilityPostgres(t *testing.T) {
	ctx, admin, dsn, cfg := setupLoginPG(t)
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
	var sessionID string
	var sessionHash [32]byte
	copy(sessionHash[:], c02cKeyHash(t))
	if err := admin.QueryRow(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,clock_timestamp()-interval '3 minutes',clock_timestamp()-interval '2 minutes',clock_timestamp()+interval '1 hour') RETURNING id::text`, issuer, sessionHash[:]).Scan(&sessionID); err != nil {
		t.Fatal("session fixture failed", safeCode(err))
	}

	t.Run("busy_session_activity_does_not_wait", func(t *testing.T) {
		release := holdLoginGate(t, ctx, admin, `SELECT id FROM app.auth_sessions WHERE id=$1 FOR SHARE`, sessionID)
		defer release()
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		session, err := loginModule.repository.LookupSession(callCtx, sessionHash, time.Now(), 30*time.Minute)
		if err != nil || session.ID != sessionID {
			t.Fatal("valid session activity blocked behind business authority lock")
		}
		var unchanged bool
		if err := witness.QueryRow(ctx, `SELECT last_seen_at<clock_timestamp()-interval '1 minute' FROM app.auth_sessions WHERE id=$1`, sessionID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("busy session activity modified the held row")
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
