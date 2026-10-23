//go:build pgx

// Responsibility: race actual API creation against transactional CSV import.
// Dependencies: disposable PG18 fixture, production router and SQL.
// Invariant: shared code lock precedes row/index locks; whole batches commit.
package catalog

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestPGCSVCreateProductSharedLock(t *testing.T) {
	for _, apiFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("apiFirst=%v", apiFirst), func(t *testing.T) {
			runner, tenant, _ := csvPGFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			admin, closeAdmin, err := pgxadapter.Open(ctx, os.Getenv("C22A_MAINTENANCE_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer closeAdmin()
			label := "csv-create-" + uuid.NewString()
			gate := int64(uuid.New().ID())
			release, ready := make(chan struct{}), make(chan struct{})
			held := make(chan error, 1)
			go func() {
				held <- admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, gate); err != nil {
						return err
					}
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			park := func(ctx context.Context, tx pgx.Tx, first bool) error {
				if !first {
					return nil
				}
				_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1::bigint)`, gate)
				return err
			}
			service := New(Config{Run: func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
				return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, label+"-api"); err != nil {
						return err
					}
					if err := fn(db.New(tx)); err != nil {
						return err
					}
					return park(ctx, tx, apiFirst)
				})
			}})
			router := testRouter(t, service, testAuth{tenant: tenant})
			apiDone, csvDone := make(chan int, 1), make(chan error, 1)
			api := func() {
				apiDone <- httpRequest(router, "POST", "/api/v1/t/"+tenant+"/products", `{"product_code":" race "}`, "").Code
			}
			csv := func() {
				csvDone <- runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, label+"-csv"); err != nil {
						return err
					}
					report, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"A-BATCH", "First", "", "en"}, []string{"RACE", "CSV title", "", "en"})))
					if err != nil {
						return err
					}
					if report.Imported != 2 {
						return fmt.Errorf("partial import: %+v", report)
					}
					return park(ctx, tx, !apiFirst)
				})
			}
			// Same advisory gate + pg_stat_activity witness protocol as
			// check_pg_races.sh. Time only bounds polling; it never orders writers.
			await := func(suffix string) {
				t.Helper()
				for {
					var event string
					err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
						return tx.QueryRow(ctx, `SELECT coalesce(max(wait_event),'') FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, label+suffix).Scan(&event)
					})
					if err != nil {
						t.Fatal(err)
					}
					if event != "" {
						if event != "advisory" {
							t.Fatalf("CSV_CREATE_SHARED_LOCK: %s waited on %s before shared code lock", suffix, event)
						}
						return
					}
					select {
					case <-ctx.Done():
						t.Fatal("CSV_CREATE_SHARED_LOCK missing wait", suffix)
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			if apiFirst {
				go api()
				await("-api")
				go csv()
				await("-csv")
			} else {
				go csv()
				await("-csv")
				go api()
				await("-api")
			}
			close(release)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			want := http.StatusConflict
			if apiFirst {
				want = http.StatusCreated
			}
			if code := <-apiDone; code != want {
				t.Fatalf("API status %d want %d", code, want)
			}
			if err := <-csvDone; err != nil {
				t.Fatal(err)
			}
			if err := runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				var products, copies int
				err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.products WHERE tenant_id=$1::uuid),(SELECT count(*) FROM app.product_content_revisions WHERE tenant_id=$1::uuid AND title IN ('First','CSV title'))`, tenant).Scan(&products, &copies)
				if err == nil && (products != 2 || copies != 2) {
					return fmt.Errorf("partial batch: products=%d copies=%d", products, copies)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
