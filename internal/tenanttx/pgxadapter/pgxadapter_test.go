//go:build pgx

// Responsibility: prove Open/InTenant/InMaintenance scope and lifecycle.
// Dependencies: DATABASE_URL for an expendable PostgreSQL (skips without it).
package pgxadapter_test

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestNilRunnerRefuses(t *testing.T) {
	var r pgxadapter.Runner
	noop := func(context.Context, pgx.Tx) error { return nil }
	if r.InMaintenance(context.Background(), noop) == nil ||
		r.InTenant(context.Background(), "11111111-1111-1111-1111-111111111111", noop) == nil {
		t.Fatal("zero Runner must refuse")
	}
}

func TestOpenScopes(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := pgxadapter.Open(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=2"); err == nil {
		t.Fatal("unreachable DSN must fail at Open")
	}
	r, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()
	setting := func(tx pgx.Tx) string {
		var v string
		if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.tenant_id', true), '')").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	const tenant = "11111111-1111-1111-1111-111111111111"
	if err = r.InTenant(ctx, tenant, func(_ context.Context, tx pgx.Tx) error {
		if got := setting(tx); got != tenant {
			t.Fatalf("InTenant scope %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.InTenant(ctx, "not-a-uuid", func(context.Context, pgx.Tx) error { return nil }); err == nil {
		t.Fatal("invalid tenant accepted")
	}
	// Pooled connections must not leak the previous transaction-local tenant.
	boom := errors.New("boom")
	if err = r.InMaintenance(ctx, func(_ context.Context, tx pgx.Tx) error {
		if got := setting(tx); got != "" {
			t.Fatalf("maintenance tx inherited tenant %q", got)
		}
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE adapter_probe(x int)"); err != nil {
			t.Fatal(err)
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("fn error not returned: %v", err)
	}
	if err = r.InMaintenance(ctx, func(_ context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename='adapter_probe'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("fn error did not roll back: n=%d %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
