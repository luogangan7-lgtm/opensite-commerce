//go:build pgx

// Responsibilities: prove cross-package fixture initialization serializes and releases.
// Dependencies: pgxadapter maintenance transactions and CI's disposable PostgreSQL.
// Invariants: observe actual advisory waiters; no sleep ordering or retries, and
// initialization errors or cancelled contexts must never leave a session lock behind.
package pgxtest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func fixtureLockRunner(t *testing.T, ctx context.Context, dsn string) *pgxadapter.IdentityRunner {
	t.Helper()
	runner, err := pgxadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		t.Fatal("fixture lock maintenance connection unavailable")
	}
	t.Cleanup(func() { _ = runner.Close(context.Background()) })
	return runner
}

func fixtureLockSource(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		Skip(t, "NOT_RUN: disposable PostgreSQL DATABASE_URL required")
	}
	return dsn
}

func TestFixtureInitializationSerializesPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	source := fixtureLockSource(t)
	holder, observer := fixtureLockRunner(t, ctx, source), fixtureLockRunner(t, ctx, source)
	locked, release, holderDone := make(chan struct{}), make(chan struct{}, 1), make(chan error, 1)
	defer close(release)
	go func() {
		holderDone <- holder.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_lock($1)", fixtureInitializationLockKey); err != nil {
				return err
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				_, _ = tx.Exec(cleanup, "SELECT pg_advisory_unlock($1)", fixtureInitializationLockKey)
			}()
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
	case err := <-holderDone:
		t.Fatal("fixture lock holder failed", err)
	case <-ctx.Done():
		t.Fatal("fixture lock holder deadline")
	}

	u, err := url.Parse(source)
	if err != nil {
		t.Fatal("fixture lock source invalid")
	}
	q := u.Query()
	q.Set("application_name", "osc_fixture_lock_waiter")
	u.RawQuery = q.Encode()
	entered, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		done <- WithFixtureInitialization(ctx, u.String(), func(context.Context) error {
			entered <- struct{}{}
			return nil
		})
	}()

	// Database observation is the synchronization condition, not elapsed time.
	for {
		select {
		case <-entered:
			t.Fatal("fixture initializer bypassed common maintenance advisory lock")
		case err := <-done:
			if err == nil {
				t.Fatal("fixture initializer bypassed common maintenance advisory lock")
			}
			t.Fatal("fixture initializer stopped before lock release", err)
		case <-ctx.Done():
			t.Fatal("fixture initializer advisory waiter not observed")
		default:
		}
		var waiting bool
		err = observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name='osc_fixture_lock_waiter' AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting)
		})
		if err != nil {
			t.Fatal("fixture initializer waiter observation failed")
		}
		if waiting {
			break
		}
	}
	release <- struct{}{}
	select {
	case err := <-holderDone:
		if err != nil {
			t.Fatal("fixture lock holder failed", err)
		}
	case <-ctx.Done():
		t.Fatal("fixture lock holder release deadline")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("fixture initialization failed after release", err)
		}
	case <-ctx.Done():
		t.Fatal("fixture initializer did not proceed after release")
	}
	select {
	case <-entered:
	default:
		t.Fatal("fixture initialization callback missing")
	}
}

func TestFixtureInitializationReleasesPostgres(t *testing.T) {
	source := fixtureLockSource(t)
	for _, mode := range []string{"success", "error", "cancel", "panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			observer := fixtureLockRunner(t, ctx, source)
			u, parseErr := url.Parse(source)
			if parseErr != nil {
				t.Fatal("fixture lock source invalid")
			}
			application := "osc_fixture_lock_release_" + mode
			q := u.Query()
			q.Set("application_name", application)
			u.RawQuery = q.Encode()
			var holderPID int32
			sentinel := errors.New("fixture initialization witness")
			recovered := false
			var err error
			func() {
				defer func() {
					if value := recover(); value != nil {
						if value != sentinel {
							t.Fatal("unexpected fixture panic")
						}
						recovered = true
					}
				}()
				err = WithFixtureInitialization(ctx, u.String(), func(context.Context) error {
					if err := observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
						return tx.QueryRow(ctx, `SELECT COALESCE((SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.application_name=$1 AND l.locktype='advisory' AND l.classid=0 AND l.objid=$2::oid AND l.objsubid=1 AND l.granted LIMIT 1),0)`, application, fixtureInitializationLockKey).Scan(&holderPID)
					}); err != nil || holderPID == 0 {
						t.Error("fixture initializer callback lacks the maintenance session advisory lock")
					}
					switch mode {
					case "error":
						return sentinel
					case "cancel":
						cancel()
						return context.Canceled
					case "panic":
						panic(sentinel)
					}
					return nil
				})
			}()
			if mode == "success" && err != nil || mode == "error" && !errors.Is(err, sentinel) || mode == "cancel" && err == nil || mode == "panic" && !recovered {
				t.Fatal("fixture initialization changed callback outcome")
			}
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			var leaked bool
			if err := observer.InMaintenance(cleanup, func(ctx context.Context, tx pgx.Tx) error {
				// Other packages may already own the shared key. Inspect only this
				// initializer's recorded backend, never require global idleness.
				return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND classid=0 AND objid=$2::oid AND objsubid=1 AND granted)`, holderPID, fixtureInitializationLockKey).Scan(&leaked)
			}); err != nil || leaked {
				t.Fatal("fixture initialization leaked maintenance session advisory lock")
			}
		})
	}
}
