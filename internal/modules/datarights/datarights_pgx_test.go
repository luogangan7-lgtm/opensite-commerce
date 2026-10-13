//go:build pgx

// Public-port gate against expendable migrated PostgreSQL. Only pgxadapter owns
// connections; fixtures use maintenance and service uses scoped tenant runner.
// Asserts artifact hashes, single use, subject fences, retry and erase cleanup.
package datarights_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestExportLifecycleAndErasure(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: expendable migrated PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, closePool, e := pgxadapter.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer closePool()
	var tenant, customer, other string
	e = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('privacy-go') RETURNING id::text").Scan(&tenant); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, "INSERT INTO app.customers(tenant_id,email,display_name) VALUES($1,'private@example.invalid','Private Name') RETURNING id::text", tenant).Scan(&customer); e != nil {
			return e
		}
		return tx.QueryRow(ctx, "INSERT INTO app.customers(tenant_id,email) VALUES($1,'other@example.invalid') RETURNING id::text", tenant).Scan(&other)
	})
	if e != nil {
		t.Fatal(e)
	}
	run := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return r.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); e != nil {
				return e
			}
			return fn(ctx, tx)
		})
	}
	root := t.TempDir()
	s, e := datarights.New(contract.Config{Run: run, Root: root, TokenKey: []byte("01234567890123456789012345678901")})
	if e != nil {
		t.Fatal(e)
	}
	id := uuid.NewString()
	actor := uuid.NewString()
	create := func(id string) {
		t.Helper()
		e = run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			out, e := s.RequestExport(ctx, tx, tenant, contract.RequestInput{ID: id, CustomerID: customer, ActorKind: "user", ActorID: actor})
			if e == nil && (out.Status != "pending" || out.DownloadToken != "") {
				t.Fatal("pending token leaked")
			}
			return e
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	create(id)
	if e = s.BuildExport(ctx, tenant, id); e != nil {
		t.Fatal(e)
	}
	if e = s.BuildExport(ctx, tenant, id); e != nil {
		t.Fatal("worker replay", e)
	}
	var status contract.Request
	e = run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		status, e = s.Status(ctx, tx, tenant, id, customer)
		return e
	})
	if e != nil || status.DownloadToken == "" || status.DownloadExpiresAt.After(time.Now().Add(15*time.Minute)) {
		t.Fatal("ready capability", e)
	}
	if _, e = s.Download(ctx, tenant, id, other, status.DownloadToken); !errors.Is(e, contract.ErrNotFound) {
		t.Fatal("subject fence", e)
	}
	p := filepath.Join(root, "data-rights", tenant, id+".json")
	original, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Download(ctx, tenant, id, customer, status.DownloadToken); !errors.Is(e, contract.ErrUnavailable) {
		t.Fatal("corrupt accepted", e)
	}
	if e = os.WriteFile(p, original, 0600); e != nil {
		t.Fatal(e)
	}
	// Two real scoped transactions compete; exactly one may consume the token.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			b, e := s.Download(ctx, tenant, id, customer, status.DownloadToken)
			if e == nil && len(b) == 0 {
				e = errors.New("empty download")
			}
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		} else if !errors.Is(e, contract.ErrConflict) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("capability race", success)
	}
	second := uuid.NewString()
	create(second)
	if e = s.BuildExport(ctx, tenant, second); e != nil {
		t.Fatal(e)
	}
	out, e := s.Erase(ctx, tenant, contract.EraseInput{CustomerID: customer, ActorKind: "user", ActorID: actor})
	if e != nil || len(out.Requests) != 2 {
		t.Fatal("erase", e)
	}
	for _, id := range out.Requests {
		if _, e = os.Stat(filepath.Join(root, "data-rights", tenant, id+".json")); !os.IsNotExist(e) {
			t.Fatal("artifact retained", e)
		}
	}
	if e = s.BuildExport(ctx, tenant, second); !errors.Is(e, contract.ErrConflict) {
		t.Fatal("erased build", e)
	}
	e = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var state, name string
		if e := tx.QueryRow(ctx, "SELECT status,display_name FROM app.customers WHERE tenant_id=$1 AND id=$2", tenant, customer).Scan(&state, &name); e != nil {
			return e
		}
		if state != "disabled" || name != "[erased]" {
			return errors.New("erasure barrier missing")
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
