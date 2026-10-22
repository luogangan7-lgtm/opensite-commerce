// Responsibilities: exercise the fixed tenant authority lane and deadline propagation.
// Dependencies: public adapter APIs and an expendable PG fixture supplied by CI.
// Invariants: four concurrent callbacks, bounded acquisition/operation, no raw pool.
package pgxadapter_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestAuthorizationStartupErrorsAreRedacted(t *testing.T) {
	const sentinel = "authority-parser-sentinel"
	for _, dsn := range []string{"postgres://user:" + sentinel + "@[invalid", "postgres://user:" + sentinel + "@127.0.0.1:1/missing?sslmode=disable"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runner, err := pgxadapter.OpenAuthorization(ctx, dsn)
		if runner != nil || err == nil || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), dsn) {
			t.Fatal("authority startup exposed connection details")
		}
	}
}

func TestAuthorizationMaintenanceBoundsPostgres(t *testing.T) {
	dsn := os.Getenv("IDENTITY_ADAPTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable PostgreSQL authority fixture required")
	}
	// Caller-supplied pool size and relaxed session defaults cannot remove bounds.
	const options = "-c lock_timeout=0 -c statement_timeout=0"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("authority fixture configuration invalid")
		}
		query := u.Query()
		query.Set("options", options)
		query.Set("pool_max_conns", "1")
		u.RawQuery = query.Encode()
		dsn = u.String()
	} else {
		dsn += " pool_max_conns=1 options='" + options + "'"
	}
	runner, err := pgxadapter.OpenAuthorization(context.Background(), dsn)
	if err != nil {
		t.Fatal("authority fixture unavailable")
	}
	defer func() { _ = runner.Close(context.Background()) }()
	t.Run("transaction_settings_and_context", func(t *testing.T) {
		if err := runner.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Error("authority callback has no internal two-second deadline")
			}
			var lock, statement, tenant, isolation string
			if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout'),current_setting('statement_timeout'),current_setting('app.tenant_id'),current_setting('transaction_isolation')`).Scan(&lock, &statement, &tenant, &isolation); err != nil {
				return err
			}
			if lock != "500ms" || statement != "1500ms" || tenant != "" || isolation != "read committed" {
				t.Error("authority transaction inherited unbounded or scoped session defaults")
			}
			return nil
		}); err != nil {
			t.Fatal("authority transaction settings unavailable")
		}
	})
	t.Run("four_connections_and_bounded_acquisition", func(t *testing.T) {
		entered := make(chan struct{}, 4)
		release := make(chan struct{})
		var workers sync.WaitGroup
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				_ = runner.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
					entered <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
		}
		defer func() { close(release); workers.Wait() }()
		readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
		defer readyCancel()
		for range 4 {
			select {
			case <-entered:
			case <-readyCtx.Done():
				t.Fatal("authority lane serialized independent callbacks")
			}
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer waitCancel()
		called := false
		err := runner.InMaintenance(waitCtx, func(context.Context, pgx.Tx) error { called = true; return nil })
		if !errors.Is(err, context.DeadlineExceeded) || called {
			t.Fatal("saturated authority lane ignored acquisition deadline")
		}
	})
	t.Run("internal_operation_deadline", func(t *testing.T) {
		started := time.Now()
		err := runner.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2500*time.Millisecond {
			t.Fatal("authority callback outlived the internal operation budget")
		}
	})
}
