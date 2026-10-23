//go:build pgx

// Responsibility: verify staff content/listing persistence and CSV/API serialization.
// Dependencies: dedicated migrated PG18 fixture, production HTTP/SQL; caller: CI.
// Invariant: product locks precede revision reads, CAS/rollback/tenant boundaries hold.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
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

func TestPGStaffContent(t *testing.T) {
	runner, a, b := csvPGFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var product, site string
	run := func(tenant string, fn func(context.Context, pgx.Tx) error) {
		t.Helper()
		if err := runner.InTenant(ctx, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"STAFF-COPY", "First", "", "en"}))); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id::text FROM app.products WHERE product_code='STAFF-COPY'`).Scan(&product); err != nil {
			return err
		}
		var org string
		if err := tx.QueryRow(ctx, `INSERT INTO app.organizations(tenant_id,display_name) VALUES($1,'content') RETURNING id::text`, a).Scan(&org); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO app.sites(tenant_id,organization_id,name) VALUES($1,$2,'content') RETURNING id::text`, a, org).Scan(&site)
	})
	s := New(Config{Run: func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { return fn(db.New(tx)) })
	}})
	r := testRouter(t, s, testAuth{tenant: a})
	foreign := testRouter(t, s, testAuth{tenant: b})
	base := "/api/v1/t/" + a + "/products/" + product
	content := base + "/content/en"
	listingPath := base + "/listings/" + site + "/en"
	body := `{"title":"Second","body_blocks":[{"kind":"text","text":"copy"}],"seo":{"title":"SEO"}}`
	expect(t, r, "GET", content, "", "", 200)
	expect(t, r, "PUT", base+"/content/fr", body, `"0"`, 200)
	expect(t, r, "PUT", base+"/content/fr", body, `"0"`, 412)
	w := expect(t, r, "PUT", content, body, `"1"`, 200)
	if w.Header().Get("ETag") != `"2"` || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing private revision headers")
	}
	expect(t, r, "PUT", content, body, `"1"`, 412)
	expect(t, r, "PUT", content, body, `"2"`, 200) // identical write is a no-op
	listingBody := `{"slug":"staff-copy","listed":true,"content_revision":2}`
	expect(t, r, "PUT", listingPath, listingBody, `"2"`, 200)
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"STAFF-COPY", "CSV next", "", "en"})))
		return err
	})
	w = expect(t, r, "GET", content, "", "", 200)
	var doc map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	if doc["revision"] != float64(3) || !strings.Contains(w.Body.String(), "SEO") {
		t.Fatalf("CSV lost content: %s", w.Body.String())
	}
	w = expect(t, r, "GET", listingPath, "", "", 200)
	if !strings.Contains(w.Body.String(), `"content_revision":2`) {
		t.Fatal("CSV changed published pointer")
	}
	expect(t, r, "PUT", listingPath, strings.ReplaceAll(listingBody, "true", "false"), `"3"`, 200)
	expect(t, foreign, "GET", strings.Replace(content, a, b, 1), "", "", 404)
	expect(t, r, "PUT", listingPath, strings.ReplaceAll(listingBody, ":2", ":999"), `"3"`, 422)
	rollback := errors.New("rollback witness")
	s.run = func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := fn(db.New(tx)); err != nil {
				return err
			}
			return rollback
		})
	}
	expect(t, r, "PUT", content, strings.Replace(body, "Second", "Rolled back", 1), `"3"`, 500)
	s.run = func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { return fn(db.New(tx)) })
	}
	w = expect(t, r, "GET", content, "", "", 200)
	if strings.Contains(w.Body.String(), "Rolled back") || w.Header().Get("ETag") != `"3"` {
		t.Fatal("rollback persisted")
	}
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM app.serving_invalidations WHERE site_id=$1 AND reason='product'`, site).Scan(&n)
		if n != 3 {
			t.Fatalf("CONTENT_INVALIDATION persisted %d want 3", n)
		}
		return err
	})
}

func TestPGContentCSVRace(t *testing.T) {
	runner, a, _ := csvPGFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var product string
	label := "content-race-" + uuid.NewString()
	if err := runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"RACE-COPY", "First", "", "en"})))
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id::text FROM app.products WHERE product_code='RACE-COPY'`).Scan(&product)
	}); err != nil {
		t.Fatal(err)
	}
	admin, closeAdmin, err := pgxadapter.Open(ctx, os.Getenv("C22A_MAINTENANCE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAdmin()
	gate := uuid.New().ID()
	ready, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, int64(gate)); err != nil {
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
	s := New(Config{Run: func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, label+"-api"); err != nil {
				return err
			}
			if err := fn(db.New(tx)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1::bigint)`, int64(gate))
			return err
		})
	}})
	r := testRouter(t, s, testAuth{tenant: a})
	path := "/api/v1/t/" + a + "/products/" + product + "/content/en"
	apiDone := make(chan int, 1)
	csvDone := make(chan error, 1)
	go func() {
		apiDone <- httpRequest(r, "PUT", path, `{"title":"API first","body_blocks":[{"kind":"text"}],"seo":{"keep":true}}`, `"1"`).Code
	}()
	// Exact fixture session names isolate advisory/row wait witnesses from other gates.
	poll := func(event string) {
		t.Helper()
		for {
			var n int
			err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$2 AND wait_event=$1`, event, label+map[string]string{"advisory": "-api", "transactionid": "-csv"}[event]).Scan(&n)
			})
			if err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("CONTENT_RACE missing " + event)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	poll("advisory")
	go func() {
		csvDone <- runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('application_name',$1,true)`, label+"-csv"); err != nil {
				return err
			}
			_, err := NewCSV().Import(ctx, tx, strings.NewReader(csvPGInput([]string{"RACE-COPY", "CSV second", "", "en"})))
			return err
		})
	}()
	poll("transactionid")
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if code := <-apiDone; code != http.StatusOK {
		t.Fatalf("CONTENT_RACE API %d", code)
	}
	if err := <-csvDone; err != nil {
		t.Fatal(err)
	}
	if err := runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var result string
		err := tx.QueryRow(ctx, `SELECT revision||'|'||title||'|'||seo::text FROM app.product_content_revisions WHERE product_id=$1 AND locale='en' ORDER BY revision DESC LIMIT 1`, product).Scan(&result)
		if !strings.HasPrefix(result, `3|CSV second|`) || !strings.Contains(result, "true") {
			return fmt.Errorf("CONTENT_RACE lost update %s", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
