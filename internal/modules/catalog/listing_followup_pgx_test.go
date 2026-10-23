//go:build pgx

// Responsibility: regress listing lost updates and serving invalidation scope.
// Dependencies: dedicated PG18 fixture, production SQL/HTTP; caller: CI pgx/races.
// Invariant: two stale writers yield one winner; persisted queue and state agree.
package catalog

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestPGListingFollowups(t *testing.T) {
	runner, a, _ := csvPGFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var product, site string
	run := func(fn func(context.Context, pgx.Tx) error) {
		t.Helper()
		if err := runner.InTenant(ctx, a, fn); err != nil {
			t.Fatal(err)
		}
	}
	run(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"LISTING", "First", "", "en"}))); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id::text FROM app.products WHERE product_code='LISTING'`).Scan(&product); err != nil {
			return err
		}
		var org string
		if err := tx.QueryRow(ctx, `INSERT INTO app.organizations(tenant_id,display_name) VALUES($1,'listing') RETURNING id::text`, a).Scan(&org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO app.sites(tenant_id,organization_id,name) VALUES($1,$2,'listing') RETURNING id::text`, a, org).Scan(&site); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) VALUES($1,$2,$3,'en','original','unpublished',1)`, a, site, product)
		return err
	})
	s := New(Config{Run: func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { return fn(db.New(tx)) })
	}})
	r := testRouter(t, s, testAuth{tenant: a})
	path := "/api/v1/t/" + a + "/products/" + product + "/listings/" + site + "/en"
	body := `{"slug":"winner","listed":true,"content_revision":1}`
	t.Run("unserved queue", func(t *testing.T) {
		run(func(ctx context.Context, tx pgx.Tx) error {
			var n int
			err := tx.QueryRow(ctx, `SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=$1 AND site_id=$2`, a, site).Scan(&n)
			if n != 0 {
				t.Errorf("UNSERVED_INVALIDATION persisted %d want 0", n)
			}
			return err
		})
	})
	t.Run("stale HTTP writers", func(t *testing.T) {
		initial := expect(t, r, "GET", path, "", "", 200).Header().Get("ETag")
		first := expect(t, r, "PUT", path, body, initial, 200)
		expect(t, r, "PUT", path, strings.ReplaceAll(body, "winner", "loser"), initial, 412)
		current := expect(t, r, "GET", path, "", "", 200)
		if first.Header().Get("ETag") == initial || !strings.Contains(current.Body.String(), `"slug":"winner"`) {
			t.Errorf("LISTING_CAS did not persist winner: %s", current.Body.String())
		}
	})
	t.Run("slug conflict rollback", func(t *testing.T) {
		run(func(ctx context.Context, tx pgx.Tx) error {
			if _, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"LISTING-OTHER", "Other", "", "en"}))); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) SELECT $1,$2,id,'en','taken','published',1 FROM app.products WHERE tenant_id=$1 AND product_code='LISTING-OTHER'`, a, site)
			return err
		})
		initial := expect(t, r, "GET", path, "", "", 200)
		w := expect(t, r, "PUT", path, strings.ReplaceAll(body, "winner", "taken"), initial.Header().Get("ETag"), 409)
		if !strings.Contains(w.Body.String(), "listing_slug_conflict") {
			t.Errorf("LISTING_SLUG wrong problem %s", w.Body.String())
		}
		current := expect(t, r, "GET", path, "", "", 200)
		if current.Body.String() != initial.Body.String() || current.Header().Get("ETag") != initial.Header().Get("ETag") {
			t.Error("LISTING_SLUG conflict mutated row")
		}
	})
	t.Run("concurrent stale HTTP writers", func(t *testing.T) {
		initial := expect(t, r, "GET", path, "", "", 200).Header().Get("ETag")
		admin, closeAdmin, err := pgxadapter.Open(ctx, os.Getenv("C22A_MAINTENANCE_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		defer closeAdmin()
		gate := int64(uuid.New().ID())
		label := "listing-race-" + uuid.NewString()
		ready, release := make(chan struct{}), make(chan struct{})
		held := make(chan error, 1)
		go func() {
			held <- admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, gate); err != nil {
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
		<-ready
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		router := func(suffix string, park bool) *Service {
			return New(Config{Run: func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
				return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, label+suffix); err != nil {
						return err
					}
					if err := fn(db.New(tx)); err != nil {
						return err
					}
					if park {
						_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, gate)
						return err
					}
					return nil
				})
			}})
		}
		poll := func(suffix, event string) {
			t.Helper()
			for {
				var n int
				err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event=$2`, label+suffix, event).Scan(&n)
				})
				if err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					return
				}
				select {
				case <-ctx.Done():
					t.Fatal("LISTING_RACE missing " + event)
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		first, second := make(chan int, 1), make(chan int, 1)
		r1, r2 := testRouter(t, router("-first", true), testAuth{tenant: a}), testRouter(t, router("-second", false), testAuth{tenant: a})
		go func() {
			first <- httpRequest(r1, "PUT", path, strings.ReplaceAll(body, "winner", "race-winner"), initial).Code
		}()
		poll("-first", "advisory")
		go func() {
			second <- httpRequest(r2, "PUT", path, strings.ReplaceAll(body, "winner", "race-loser"), initial).Code
		}()
		poll("-second", "transactionid")
		close(release)
		if err := <-held; err != nil {
			t.Fatal(err)
		}
		one, two := <-first, <-second
		if one != 200 || two != 412 {
			t.Errorf("LISTING_RACE statuses %d/%d want 200/412", one, two)
		}
		run(func(ctx context.Context, tx pgx.Tx) error {
			var slug string
			err := tx.QueryRow(ctx, `SELECT slug FROM app.product_listings WHERE tenant_id=$1 AND site_id=$2 AND product_id=$3 AND locale='en'`, a, site, product).Scan(&slug)
			if slug != "race-winner" {
				return fmt.Errorf("LISTING_RACE persisted %s", slug)
			}
			return err
		})
	})
}
