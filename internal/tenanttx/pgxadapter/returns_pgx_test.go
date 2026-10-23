//go:build pgx

// Responsibility: exercise coordinator login rejection on disposable CI PostgreSQL.
// Dependencies: pgxadapter maintenance fixture and real capability catalogs.
// Invariants: exact group only; missing authorization never reaches callbacks.
package pgxadapter_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestReturnsCoordinatorDedicatedLoginPostgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	login := "c19c_" + uuid.NewString()[:8]
	guardURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	query := guardURL.Query()
	query.Set("application_name", login+"_guard")
	guardURL.RawQuery = query.Encode()
	guard, err := pgxadapter.OpenIdentity(ctx, guardURL.String())
	if err != nil {
		t.Fatal("fixture lock connection unavailable")
	}
	defer guard.Close(context.Background())
	// Migrations in other databases inspect these cluster-global memberships.
	// Hold the existing COMMON-source lock until the temporary role is gone.
	if err := pgxtest.WithFixtureInitialization(ctx, guard.InMaintenance, func(ctx context.Context) error {
		testReturnsCoordinatorDedicatedLogin(t, ctx, dsn, login)
		return nil
	}); err != nil {
		t.Fatal("returns fixture initialization failed", err)
	}
}

func testReturnsCoordinatorDedicatedLogin(t *testing.T, ctx context.Context, dsn, login string) {
	t.Helper()
	admin, err := pgxadapter.OpenAuthorization(ctx, dsn)
	if err != nil {
		t.Fatal("maintenance fixture unavailable")
	}
	defer admin.Close(context.Background())
	// A database per fixture does not isolate pg_auth_members. Witness this
	// fixture's own common-database lock before publishing cluster membership.
	assertFixtureLock := func() {
		t.Helper()
		var held bool
		err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE a.application_name=$1 AND l.locktype='advisory'
 AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND l.classid=0 AND l.objid=18004518 AND l.objsubid=1
 AND l.mode='ExclusiveLock' AND l.granted)`, login+"_guard").Scan(&held)
		})
		if err != nil || !held {
			t.Error("returns fixture cluster membership lacks common initialization lock")
		}
	}
	assertFixtureLock()
	exec := func(sql string) {
		t.Helper()
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE ROLE " + login + " LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD 'ci_returns_fixture_only_123456789'; GRANT app_returns_service TO " + login)
	defer func() {
		assertFixtureLock()
		exec("DROP OWNED BY " + login + "; DROP ROLE " + login)
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	exec("GRANT CONNECT ON DATABASE " + pgx.Identifier{u.Path[1:]}.Sanitize() + " TO " + login)
	u.User = url.UserPassword(login, "ci_returns_fixture_only_123456789")
	open := func() (*pgxadapter.QueryRunner, error) { return pgxadapter.OpenReturnsRunner(ctx, u.String()) }
	runner, err := open()
	if err != nil {
		t.Fatal("dedicated returns login rejected:", err)
	}
	called := false
	if err := runner.RunAuthorizedTx(ctx, "11111111-1111-4111-8111-111111111111", func(context.Context, pgx.Tx) error { called = true; return nil }); err == nil || called {
		t.Fatal("unauthenticated returns callback admitted")
	}
	runner.Close()
	t.Setenv("OPENSITE_RETURNS_DATABASE_URL", u.String())
	config, closeRuntime, err := bootstrap.FromEnvironment(ctx, bootstrap.Config{})
	if err != nil {
		t.Fatal("coordinator bootstrap rejected:", err)
	}
	if config.ReturnsCoordinator.Runner == nil || config.Returns.Service != nil {
		closeRuntime()
		t.Fatal("returns binding missing or enabled refund without trusted readers")
	}
	closeRuntime()
	for _, fault := range []struct{ add, undo string }{
		{"GRANT app_runtime TO " + login, "REVOKE app_runtime FROM " + login},
		{"GRANT SELECT(password_hash) ON app.users TO " + login, "REVOKE SELECT(password_hash) ON app.users FROM " + login},
		{"ALTER ROLE " + login + " BYPASSRLS", "ALTER ROLE " + login + " NOBYPASSRLS"},
		{"ALTER ROLE " + login + " NOINHERIT", "ALTER ROLE " + login + " INHERIT"},
	} {
		exec(fault.add)
		bad, err := open()
		if bad != nil {
			bad.Close()
		}
		exec(fault.undo)
		if err == nil {
			t.Fatal("unsafe coordinator capability accepted")
		}
	}
	runner, err = open()
	if err != nil {
		t.Fatal("restored coordinator rejected")
	}
	runner.Close()
}
