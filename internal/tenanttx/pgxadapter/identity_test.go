// Responsibilities: exercise identity adapter fail-closed startup and API shape.
// Dependencies: pgx and the actual DSN parser; callers: default/pgx Go tests.
// Invariants: parser/connect secrets stay private and no direct DBTX/Begin port.
package pgxadapter_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestIdentityStartupErrorsAreRedacted(t *testing.T) {
	const secret = "identity-parser-secret-sentinel"
	for i, dsn := range []string{"postgres://user:" + secret + "@[invalid", "postgres://user:" + secret + "@127.0.0.1:1/missing?sslmode=disable"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, err := pgxadapter.OpenIdentity(ctx, dsn)
		if r != nil || err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dsn) {
			t.Fatal("identity startup error exposed connection details")
		}
		want := []string{"identity database configuration failed", "identity database connection failed"}[i]
		if err.Error() != want {
			t.Fatal("identity startup returned a driver error instead of the public error")
		}
	}
}

func TestIdentityRunnerExposesOnlyCheckedTransactions(t *testing.T) {
	var r *pgxadapter.IdentityRunner
	called := false
	if err := r.InMaintenance(context.Background(), func(context.Context, pgx.Tx) error { called = true; return nil }); err == nil || called {
		t.Fatal("nil runner accepted work")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var value pgxadapter.IdentityRunner
	if _, ok := any(&value).(db.DBTX); ok {
		t.Fatal("runner exposes DBTX")
	}
	if _, ok := any(&value).(interface {
		Begin(context.Context) (pgx.Tx, error)
	}); ok {
		t.Fatal("runner exposes Begin")
	}
}

// The operator supplies an expendable login with TEMP privilege. This test
// checks real pgx transactions only; bootstrap role authority has its SQL gate.
func TestIdentityMaintenanceTransaction(t *testing.T) {
	dsn := os.Getenv("IDENTITY_ADAPTER_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL") // CI's expendable PG; TEMP privilege suffices
	}
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable PostgreSQL IDENTITY_ADAPTER_TEST_DATABASE_URL or DATABASE_URL required")
	}
	// Challenge both guarantees with hostile session defaults on the LOGIN.
	const options = "-c default_transaction_isolation=serializable -c app.tenant_id=11111111-1111-1111-1111-111111111111"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test database configuration")
		}
		q := u.Query()
		q.Set("options", options)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " options='" + options + "'"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := pgxadapter.OpenIdentity(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Close(closeCtx)
	}()
	boom := errors.New("rollback witness")
	if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var isolation, tenant string
		if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation'), current_setting('app.tenant_id',true)").Scan(&isolation, &tenant); err != nil {
			return err
		}
		if isolation != "read committed" || tenant != "" {
			t.Fatalf("identity transaction inherited defaults: isolation=%q tenant=%q", isolation, tenant)
		}
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE identity_adapter_rollback_probe(x int)"); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal("callback rollback failed")
	}
	if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var absent bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass('pg_temp.identity_adapter_rollback_probe') IS NULL").Scan(&absent); err != nil {
			return err
		}
		if !absent {
			t.Fatal("callback failure committed temporary table")
		}
		return nil
	}); err != nil {
		t.Fatal("identity transaction verification failed")
	}
}
