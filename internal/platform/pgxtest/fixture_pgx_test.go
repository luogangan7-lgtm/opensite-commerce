//go:build pgx

// Responsibilities: prove cross-package fixture initialization serializes and releases.
// Dependencies: pgxadapter maintenance transactions and CI's disposable PostgreSQL.
// Invariants: observe actual advisory waiters; no sleep ordering or retries, and
// initialization errors or cancelled contexts must never leave a session lock behind.
package pgxtest

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/url"
	"os"
	"strings"
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

// Each caller owns and closes the physical maintenance session, including panic
// and cancellation. The shared helper owns the only lock implementation.
func fixtureLockInitialize(ctx context.Context, source string, initialize func(context.Context) error) error {
	runner, err := pgxadapter.OpenIdentity(ctx, source)
	if err != nil {
		return errors.New("fixture lock maintenance connection unavailable")
	}
	defer runner.Close(context.Background())
	return WithFixtureInitialization(ctx, runner.InMaintenance, initialize)
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
		done <- fixtureLockInitialize(ctx, u.String(), func(context.Context) error {
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
				err = fixtureLockInitialize(ctx, u.String(), func(context.Context) error {
					if err := observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
						return tx.QueryRow(ctx, `SELECT COALESCE((SELECT a.pid FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.application_name=$1 AND l.locktype='advisory' AND l.classid=0 AND l.objid::bigint=$2 AND l.objsubid=1 AND l.granted LIMIT 1),0)`, application, fixtureInitializationLockKey).Scan(&holderPID)
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
				return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND classid=0 AND objid::bigint=$2 AND objsubid=1 AND granted)`, holderPID, fixtureInitializationLockKey).Scan(&leaked)
			}); err != nil || leaked {
				t.Fatal("fixture initialization leaked maintenance session advisory lock")
			}
		})
	}
}

// This contract also powers CI mutation probes: wrong lock key and cancelled
// cleanup context must both fail, while the restored helper must pass.
func TestFixtureRolesLifecyclePostgres(t *testing.T) {
	source := fixtureLockSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	observer := fixtureLockRunner(t, ctx, source)
	name := "osc_role_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var pid uint32
	t.Run("cancelled owner", func(t *testing.T) {
		owner, stop := context.WithCancel(ctx)
		defer stop()
		r := NewRole(t, owner, source, name, uuid.NewString(), true, "app_runtime")
		// Multiple bootstrap bindings must reuse one backend, not deadlock.
		second := NewRole(t, owner, source, name+"_two", uuid.NewString(), true, "app_auth")
		if r.session != second.session {
			t.Fatal("fixture roles did not reuse test lock")
		}
		pid = r.session.conn.PgConn().PID()
		var held bool
		if err := observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_locks WHERE pid=$1 AND locktype='advisory'
    AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
    AND classid=0 AND objid=18004518 AND objsubid=1 AND mode='ExclusiveLock' AND granted)`, pid).Scan(&held)
		}); err != nil || !held {
			t.Fatal("role fixture lacks common database lock", err)
		}
		if err := observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "CREATE TABLE public."+pgx.Identifier{name}.Sanitize()+"(id int); ALTER TABLE public."+pgx.Identifier{name}.Sanitize()+" OWNER TO "+pgx.Identifier{name}.Sanitize())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		stop() // Both role cleanup and session close must survive owner cancellation.
	})
	// pgx.Close sends Terminate and closes the socket; PostgreSQL may still be
	// releasing the backend's session lock after the Go cleanups have returned.
	// Observe server completion before the unchanged leak assertion; do not retry
	// role cleanup or ignore any role/object/lock leak once that backend is gone.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var active bool
		if err := observer.InMaintenance(cleanup, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_stat_activity WHERE pid=$1)", pid).Scan(&active)
		}); err != nil {
			t.Fatal("role fixture cleanup observation failed", err)
		}
		if !active {
			break
		}
		select {
		case <-cleanup.Done():
			t.Fatal("role fixture backend did not finish cleanup", cleanup.Err())
		case <-tick.C:
		}
	}
	var leaked bool
	if err := observer.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Socket Close can return before PostgreSQL processes the disconnect.
		// Taking the same lock observes completed teardown without sleep/polling.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", fixtureInitializationLockKey); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_roles WHERE rolname IN ($1,$2))
   OR to_regclass('public.' || $1) IS NOT NULL
   OR EXISTS(SELECT FROM pg_locks WHERE pid=$3 AND locktype='advisory' AND classid=0 AND objid=18004518)`, name, name+"_two", pid).Scan(&leaked)
	}); err != nil || leaked {
		t.Fatal("role fixture leaked roles, owned objects or lock after cancellation", err)
	}
}
