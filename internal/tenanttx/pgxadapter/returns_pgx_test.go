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
	admin, err := pgxadapter.OpenAuthorization(ctx, dsn)
	if err != nil {
		t.Fatal("maintenance fixture unavailable")
	}
	defer admin.Close(context.Background())
	fixtureRole := pgxtest.NewRole(t, ctx, dsn, login, "ci_returns_fixture_only_123456789", true, "app_returns_service")
	exec := func(sql string) {
		t.Helper()
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
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
		{"GRANT SELECT(password_hash) ON app.users TO " + login, "REVOKE SELECT(password_hash) ON app.users FROM " + login},
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
	for _, attribute := range []struct{ add, undo string }{{"BYPASSRLS", "NOBYPASSRLS"}, {"NOINHERIT", "INHERIT"}, {"membership", ""}} {
		if attribute.add == "membership" {
			fixtureRole.Grant(t, ctx, "app_runtime", "")
		} else {
			fixtureRole.Alter(t, ctx, attribute.add)
		}
		bad, err := open()
		if bad != nil {
			bad.Close()
		}
		if attribute.add == "membership" {
			fixtureRole.Revoke(t, ctx, "app_runtime")
		} else {
			fixtureRole.Alter(t, ctx, attribute.undo)
		}
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
