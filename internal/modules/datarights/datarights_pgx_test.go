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
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
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
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL required")
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
	rotated, err := datarights.New(contract.Config{Run: run, Root: root, TokenKey: []byte("11234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	if err = run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		out, err := rotated.Status(ctx, tx, tenant, id, customer)
		if err == nil && out.DownloadToken != "" {
			return errors.New("rotated key advertised a capability absent from the database")
		}
		return err
	}); err != nil {
		t.Fatal(err)
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
	// A built file may survive a failed DB commit. Its row remains pending,
	// so the old token cannot reach it; a sweep must remove that orphan.
	rollbackID := uuid.NewString()
	create(rollbackID)
	rollbackErr := errors.New("injected commit rollback")
	rolled, err := datarights.New(contract.Config{Root: root, TokenKey: []byte("01234567890123456789012345678901"), Run: func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return rollbackErr
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = rolled.BuildExport(ctx, tenant, rollbackID); !errors.Is(err, rollbackErr) {
		t.Fatal("rollback injection", err)
	}
	if _, err = s.Download(ctx, tenant, rollbackID, customer, "uncommitted"); err == nil {
		t.Fatal("rolled-back export reachable")
	}
	// Final and temporary artifacts with no row, including a rolled-back request.
	missing := uuid.NewString()
	for _, name := range []string{missing + ".json", missing + ".json." + uuid.NewString() + ".tmp"} {
		if err := os.WriteFile(filepath.Join(root, "data-rights", tenant, name), []byte("orphan PII"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sweeper, ok := s.(interface {
		Sweep(context.Context, string) error
	})
	if !ok {
		t.Fatal("privacy artifact sweep missing")
	}
	for range 2 {
		if err := sweeper.Sweep(ctx, tenant); err != nil {
			t.Fatal("idempotent sweep", err)
		}
	}
	for _, orphan := range []string{rollbackID, missing} {
		if _, err := os.Stat(filepath.Join(root, "data-rights", tenant, orphan+".json")); !os.IsNotExist(err) {
			t.Fatal("orphan PII retained", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "data-rights", tenant))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatal("temporary PII retained")
		}
	}
	expired := uuid.NewString()
	create(expired)
	if err = s.BuildExport(ctx, tenant, expired); err != nil {
		t.Fatal(err)
	}
	if err = sweeper.Sweep(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "data-rights", tenant, expired+".json")); err != nil {
		t.Fatal("live artifact removed", err)
	}
	var expiredStatus contract.Request
	if err = run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		expiredStatus, err = s.Status(ctx, tx, tenant, expired, customer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.data_rights_exports SET download_expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, expired)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Download(ctx, tenant, expired, customer, expiredStatus.DownloadToken); !errors.Is(err, contract.ErrConflict) {
		t.Fatal("expired export reachable", err)
	}
	if err = sweeper.Sweep(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "data-rights", tenant, expired+".json")); !os.IsNotExist(err) {
		t.Fatal("expired PII retained", err)
	}
	second := uuid.NewString()
	create(second)
	if e = s.BuildExport(ctx, tenant, second); e != nil {
		t.Fatal(e)
	}
	out, e := s.Erase(ctx, tenant, contract.EraseInput{CustomerID: customer, ActorKind: "user", ActorID: actor})
	if e != nil || len(out.Requests) != 4 {
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

// Permanent errors must be returned only after the terminal metadata commits.
// The worker cancels these errors, so leaving pending loses every retry path.
func TestPermanentExportFailuresCommitTerminalState(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()
	run := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return r.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	s, err := datarights.New(contract.Config{Run: run, Root: t.TempDir(), TokenKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state string
		want        error
	}{
		{"expired", "expired", contract.ErrConflict},
		{"records", "failed", contract.ErrInvalid},
		{"bytes", "failed", contract.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tenant, customer string
			id := uuid.NewString()
			if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('privacy-terminal-go') RETURNING id::text").Scan(&tenant); err != nil {
					return err
				}
				return tx.QueryRow(ctx, "INSERT INTO app.customers(tenant_id,email) VALUES($1,'terminal@example.invalid') RETURNING id::text", tenant).Scan(&customer)
			}); err != nil {
				t.Fatal(err)
			}
			if err := run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := s.RequestExport(ctx, tx, tenant, contract.RequestInput{ID: id, CustomerID: customer, ActorKind: "user", ActorID: uuid.NewString()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				switch tc.name {
				case "expired":
					_, err = tx.Exec(ctx, "UPDATE app.data_rights_exports SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, id)
				case "records":
					_, err = tx.Exec(ctx, "INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code) SELECT $1,$2,'Private','Private','Private','US' FROM generate_series(1,10001)", tenant, customer)
				case "bytes":
					_, err = tx.Exec(ctx, "UPDATE app.customers SET display_name=repeat('x',10485761) WHERE tenant_id=$1 AND id=$2", tenant, customer)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.BuildExport(ctx, tenant, id); !errors.Is(err, tc.want) {
					t.Fatalf("permanent build error = %v, want %v", err, tc.want)
				}
				if err := r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
					var state string
					if err := tx.QueryRow(ctx, "SELECT status FROM app.data_rights_exports WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&state); err != nil {
						return err
					}
					if state != tc.state {
						return errors.New("permanent failure did not commit terminal state: " + state)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
