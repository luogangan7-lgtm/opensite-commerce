//go:build pgx

// Responsibility: real dedicated-login end-to-end public read/role rejection.
// Dependencies: pgxadapter, migrated expendable DATABASE_URL and C09 SQL fixture.
// Caller: pgx gate; invariant: DATABASE_URL never permits skipped public acceptance.
package publish_test

import (
	"context"
	"encoding/json"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPublicPublishedReadPG(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: migrated expendable DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, closeAdmin, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAdmin()
	if _, close, err := pgxadapter.OpenPublic(ctx, dsn); err == nil {
		close()
		t.Fatal("privileged fixture DSN accepted for public reads")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "storefront_seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "\\") || strings.HasPrefix(line, "BEGIN ISOLATION") || line == "COMMIT;" {
			continue
		}
		lines = append(lines, line)
	}
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, strings.Join(lines, "\n"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(secret)
	role := "c09_public_" + suffix[:12]
	password := suffix
	quoted := pgx.Identifier{role}.Sanitize()
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '"+password+"'; GRANT app_domain_resolver TO "+quoted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, "DROP ROLE "+quoted); return err }); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatal("test DATABASE_URL must be URL form")
	}
	u.User = url.UserPassword(role, password)
	publicDSN := u.String()
	module, close, err := publish.Open(ctx, publicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	rawStore, closeStore, err := store.Open(ctx, publicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	got, err := module.Current(ctx, "LOCALHOST.")
	if err != nil || got.Title != "OpenSite sample shop" || len(got.Pages) != 3 || len(got.Products) != 2 {
		t.Fatalf("public snapshot: %+v %v", got, err)
	}
	wantProducts := []contract.Product{
		{ID: "c0900007-0000-4000-8000-000000000001", Slug: "canvas-tote", Name: "Canvas tote", Description: "A reusable cotton tote for daily essentials.", UnitAmount: "2400", Currency: "USD"},
		{ID: "c0900007-0000-4000-8000-000000000002", Slug: "ceramic-mug", Name: "Ceramic mug", Description: "A simple mug for your morning routine.", UnitAmount: "1800", Currency: "USD"},
	}
	if !reflect.DeepEqual(got.Products, wantProducts) {
		t.Fatalf("published catalog snapshot: %+v", got.Products)
	}
	// Commit a catalog publication change: a fresh public read must still use
	// the sealed release, rather than joining mutable listings or draft revisions.
	setCatalogPointer := func(ctx context.Context, revision int64) error {
		return admin.InTenant(ctx, "c0900001-0000-4000-8000-000000000001", func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE app.product_listings SET published_content_revision=$1
				WHERE tenant_id=$2 AND site_id=$3 AND product_id=$4 AND locale='en' AND status='published'`,
				revision, "c0900001-0000-4000-8000-000000000001", "c0900003-0000-4000-8000-000000000001", "c0900007-0000-4000-8000-000000000001")
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("catalog pointer changed %d rows", tag.RowsAffected())
			}
			return nil
		})
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := setCatalogPointer(cleanup, 1); err != nil {
			t.Errorf("restore catalog fixture pointer: %v", err)
		}
	}()
	if err := setCatalogPointer(ctx, 2); err != nil {
		t.Fatal(err)
	}
	fresh, err := module.Current(ctx, "localhost")
	if err != nil || fresh.ReleaseID != got.ReleaseID || !reflect.DeepEqual(fresh.Products, wantProducts) {
		t.Fatalf("mutable catalog change leaked into sealed release: %+v %v", fresh.Products, err)
	}
	if err := setCatalogPointer(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"unknown.c09.test", "pending.c09.test", "disabled.c09.test", "unpublished.c09.test"} {
		if _, err := module.Current(ctx, host); !errors.Is(err, contract.ErrNotFound) {
			t.Fatalf("%s: %v", host, err)
		}
	}
	for _, p := range got.Pages {
		if strings.Contains(p.Title, "SECRET") {
			t.Fatal("draft revision leaked")
		}
	}
	other, err := module.Current(ctx, "other.c09.test")
	if err != nil || other.Title != "OTHER_TENANT_SECRET" || other.SiteID == got.SiteID {
		t.Fatalf("host scope: %+v %v", other, err)
	}
	r, closeR, err := pgxadapter.OpenPublic(ctx, publicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeR()
	if err = r.InPublicRead(ctx, "localhost", func(ctx context.Context, tx pgx.Tx) error {
		var readonly, isolation string
		if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readonly); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
			return err
		}
		if readonly != "on" || isolation != "repeatable read" {
			t.Fatalf("unsafe public transaction: %s %s", readonly, isolation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"SET FALSE", "ADMIN TRUE"} {
		if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "GRANT app_domain_resolver TO "+quoted+" WITH "+option)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, close, err := pgxadapter.OpenPublic(ctx, publicDSN); err == nil {
			close()
			t.Errorf("unsafe resolver membership option accepted: %s", option)
		}
		if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "GRANT app_domain_resolver TO "+quoted+" WITH SET TRUE, ADMIN FALSE")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Review thread rnC5D: a second grantor's ADMIN membership must also be rejected.
	provisioner := pgx.Identifier{"c09_prov_" + suffix[:12]}.Sanitize()
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE ROLE "+provisioner+" NOLOGIN; GRANT app_domain_resolver TO "+provisioner+" WITH ADMIN OPTION; GRANT app_domain_resolver TO "+quoted+" WITH ADMIN OPTION GRANTED BY "+provisioner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, close, err := pgxadapter.OpenPublic(ctx, publicDSN); err == nil {
		close()
		t.Error("ADMIN resolver membership from a second grantor accepted")
	}
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "REVOKE app_domain_resolver FROM "+quoted+" GRANTED BY "+provisioner+"; DROP ROLE "+provisioner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, close, err := pgxadapter.OpenPublic(ctx, publicDSN); err != nil {
		t.Fatalf("single safe membership rejected after revoking second grantor: %v", err)
	} else {
		close()
	}
	// Review threads rnC40/rnFiU/rnFib: a >50 KB pinned page stays published with or
	// without a navigation link, and the sealed manifest (not sites.default_locale) selects routes.
	for _, nav := range []string{`[]`, `[{"label":"About","href":"/about"}]`} {
		tenant, host := newUUID(t), "large-"+newUUID(t)[:8]+".c09.test"
		if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(largeSiteSQL, tenant, host, nav, 1, 7, "en", 9000))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		large, err := module.Current(ctx, host)
		if err != nil || len(large.Pages) != 1 || large.Pages[0].Path != "/about" || len(large.Pages[0].Blocks) != 7 {
			t.Fatalf("valid >50 KB published page dropped (nav %s): pages=%d %v", nav, len(large.Pages), err)
		}
		if err = admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE app.sites SET default_locale='fr' WHERE tenant_id=$1`, tenant)
			if err == nil && tag.RowsAffected() != 1 {
				err = fmt.Errorf("default_locale changed %d rows", tag.RowsAffected())
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if after, err := module.Current(ctx, host); err != nil || after.Locale != "en" || len(after.Pages) != 1 {
			t.Fatalf("mutable default_locale changed sealed release read: %+v %v", after.Locale, err)
		}
	}
	// Review thread rtzAC: an over-budget snapshot (four ~630 KB pages, > 2 MiB pre-bound) is rejected by the
	// store before pages are aggregated or transferred; rtvb1: lowercase locale is canonical.
	for _, tc := range []struct {
		pages, blocks, text int
		locale              string
		oversized           bool
	}{{4, 70, 9000, "en", true}, {1, 7, 9000, "en-us", false},
		// ruH5Z: 100x200 blocks of 20 chars is ~1.03 MB compact JSON (served) but ~1.1 MB as
		// jsonb::text; 22 chars is ~1.07 MB compact, so Project's exact check rejects it.
		{100, 200, 20, "en", false}, {100, 200, 22, "en", true}} {
		tenant, host := newUUID(t), "budget-"+newUUID(t)[:8]+".c09.test"
		if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(largeSiteSQL, tenant, host, `[]`, tc.pages, tc.blocks, tc.locale, tc.text))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, _, _, _, pages, err := rawStore.Current(ctx, host)
		if tc.oversized && tc.text == 22 {
			if _, err := module.Current(ctx, host); !errors.Is(err, contract.ErrUnavailable) {
				t.Errorf("compact response just over 1 MiB served: %v", err)
			}
			continue
		}
		if tc.oversized {
			if !errors.Is(err, contract.ErrUnavailable) || len(pages) != 0 {
				t.Errorf("over-budget snapshot not rejected before aggregation: err=%v pages=%d bytes", err, len(pages))
			}
			if _, err := module.Current(ctx, host); !errors.Is(err, contract.ErrUnavailable) {
				t.Errorf("over-budget snapshot served: %v", err)
			}
			continue
		}
		got, err := module.Current(ctx, host)
		if err != nil || got.Locale != map[string]string{"en": "en", "en-us": "en-US"}[tc.locale] || len(got.Pages) != tc.pages {
			t.Errorf("valid release (%d pages, %d-char blocks, %s) not served: %+v %v", tc.pages, tc.text, tc.locale, got.Locale, err)
		} else if encoded, _ := json.Marshal(got); tc.pages == 100 && len(encoded) < 1_000_000 {
			t.Errorf("near-limit fixture too small: %d bytes", len(encoded))
		}
	}
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "GRANT app_runtime TO "+quoted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, close, err := pgxadapter.OpenPublic(ctx, publicDSN); err == nil {
		close()
		t.Fatal("mixed runtime/public membership accepted")
	}
}

func newUUID(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// largeSiteSQL publishes one sealed release. Args: tenant uuid, verified host,
// navigation JSON, page count, paragraphs per page, release locale, paragraph length.
// The first page is /about (7 blocks is about 63 KB); others are /about-<n>.
const largeSiteSQL = `DO $large$
DECLARE t uuid := '%[1]s'; o uuid := gen_random_uuid(); s uuid := gen_random_uuid();
 pg uuid; rel uuid := gen_random_uuid(); m jsonb; i int;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'C09 large page '||t);
 PERFORM set_config('app.tenant_id',t::text,true);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,o,'C09 large');
 INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(t,s,o,'Large','%[6]s');
 m := jsonb_build_object('version',1,'locale','%[6]s','title','Large','description','',
  'navigation','%[3]s'::jsonb,'products','[]'::jsonb);
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,s,rel,1,'c09-review',m,sha256(convert_to(m::text,'UTF8')));
 FOR i IN 1..%[4]d LOOP
  pg := gen_random_uuid();
  INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(t,s,pg,'about'||i);
  INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES(t,s,pg,'%[6]s',1,
   jsonb_build_object('title','About','description','','blocks',
    (SELECT jsonb_agg(jsonb_build_object('kind','paragraph','text',repeat('x',%[7]d))) FROM generate_series(1,%[5]d))));
  INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
   VALUES(t,s,rel,'%[6]s',CASE WHEN i=1 THEN '/about' ELSE '/about-'||i END,pg,1);
 END LOOP;
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_domain_verifier';
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES(t,s,'%[2]s','verified',now());
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_publisher';
 PERFORM app.publish_release(t,s,rel,NULL);
 EXECUTE 'RESET ROLE';
END $large$`
