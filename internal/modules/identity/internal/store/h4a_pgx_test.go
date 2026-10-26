//go:build pgx

// Responsibility: witness session isolation with controllable credential occupancy.
// Dependencies: the production private constructor, real auth pools/SQL and disposable CI PG.
// Invariants: a held credential callback cannot occupy the independent session pool;
// caller queue deadlines remain bounded. Production lock/operation budgets are tested separately.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type occupiedAuthRunner struct {
	authRunner
	hold             atomic.Bool
	entered, release chan struct{}
}

func (r *occupiedAuthRunner) RunAuthentication(ctx context.Context, fn func(context.Context, *db.Queries) error) error {
	return r.authRunner.RunAuthentication(ctx, func(ctx context.Context, q *db.Queries) error {
		if r.hold.CompareAndSwap(true, false) {
			close(r.entered)
			// Test-owned barrier deliberately holds the acquired connection even
			// after its operation context expires. No SQL lock or budget is changed.
			<-r.release
		}
		return fn(ctx, q)
	})
}

func TestH4ARealSessionQueueIsolationPostgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(entropy[:])
	role := "osc_h4a_" + suffix
	pgxtest.NewRole(t, ctx, dsn, role, suffix, true, "app_auth")
	admin, err := pgxadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	hash := sha256.Sum256(entropy[:])
	var userID, sessionID string
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO app.users(email,password_hash) VALUES($1,'fixture') RETURNING id::text", suffix+"@h4a.test").Scan(&userID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') RETURNING id::text", userID, hash[:]).Scan(&sessionID)
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "DELETE FROM app.auth_sessions WHERE user_id=$1", userID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DELETE FROM app.users WHERE id=$1", userID)
			return err
		}); err != nil {
			t.Error("fixture cleanup", err)
		}
	}()
	authURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	authURL.User = url.UserPassword(role, suffix)
	var credential *occupiedAuthRunner
	opens := 0
	repo, err := openLogin(ctx, authURL.String(), 30*time.Minute, func(ctx context.Context, dsn string) (authRunner, error) {
		runner, err := pgxadapter.OpenStaffAuthentication(ctx, dsn)
		opens++
		if opens == 1 {
			credential = &occupiedAuthRunner{authRunner: runner, entered: make(chan struct{}), release: make(chan struct{})}
			return credential, err
		}
		return runner, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close(context.Background())
	if opens != 2 {
		t.Fatal("session and credential lanes must be opened independently")
	}
	var once sync.Once
	release := func() { once.Do(func() { close(credential.release) }) }
	defer release()
	credential.hold.Store(true)
	done := make(chan error, 1)
	go func() { _, err := repo.Credential(ctx, suffix+"@h4a.test"); done <- err }()
	select {
	case <-credential.entered:
	case err := <-done:
		t.Fatal("credential occupancy failed", err)
	case <-ctx.Done():
		t.Fatal("credential readiness deadline", ctx.Err())
	}
	queuedCtx, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	_, err = repo.Credential(queuedCtx, suffix+"@h4a.test")
	stop()
	if !errors.Is(err, login.ErrDatabase) {
		t.Fatal("credential queue ignored caller deadline", err)
	}
	results := make(chan error, 8)
	floodCtx, stopFlood := context.WithTimeout(ctx, 3*time.Second)
	defer stopFlood()
	for i := 0; i < cap(results); i++ {
		go func() {
			session, err := repo.LookupSession(floodCtx, hash, time.Now(), 30*time.Minute)
			if err == nil && (session.ID != sessionID || session.UserID != userID || session.DatabaseNow.IsZero()) {
				err = login.ErrSession
			}
			results <- err
		}()
	}
	for i := 0; i < cap(results); i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Error("session flood queued behind credential lane", err)
			}
		case <-floodCtx.Done():
			t.Fatal("session flood deadline", floodCtx.Err())
		}
	}
	select {
	case err := <-done:
		t.Fatal("credential occupancy ended before explicit release", err)
	default:
	}
	release()
	select {
	case err := <-done:
		// A slow CI runner may cross the unchanged 2s budget while the
		// test-owned callback holds the connection. It must still release.
		if err != nil && !errors.Is(err, login.ErrDatabase) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("credential release deadline", ctx.Err())
	}
}
