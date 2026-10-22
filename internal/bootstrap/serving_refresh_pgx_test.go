//go:build pgx

// Responsibility: drive the storefront serving refresher through the production
// QueryRunner.RunTrustedTx seam (ADR G11, G13 composition) with a dedicated runtime login,
// and prove a refreshed row keeps SKUs purchasable past its lifetime.
// Dependencies: disposable migrated DATABASE_URL, pgxadapter, publish facade.
package bootstrap_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func refreshUUID(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// refreshSiteSQL publishes a release whose product has one priced, stocked, sealed SKU.
// Args: tenant, site, product, sku, host.
const refreshSiteSQL = `DO $refresh$
DECLARE t uuid := '%[1]s'; o uuid := gen_random_uuid(); s uuid := '%[2]s'; p uuid := '%[3]s'; v uuid := '%[4]s';
 pg uuid := gen_random_uuid(); rel uuid := gen_random_uuid(); m jsonb;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'serving refresh '||t);
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,o,'Refresh');
 INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(t,s,o,'Refresh','en');
 INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES(t,p,'BOOT-P','ready');
 INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES(t,v,p,'BOOT-1','one','ready');
 WITH l AS (INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority,audience) VALUES(t,'USD','active',now()-interval '1 day',0,'all') RETURNING id)
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) SELECT t,l.id,v,1,500 FROM l;
 PERFORM app.inventory_adjust(v,5,'refresh fixture',gen_random_uuid(),'inbound');
 m := jsonb_build_object('version',1,'locale','en','title','Refresh','description','','navigation','[]'::jsonb,
  'products',jsonb_build_array(jsonb_build_object('id',p::text,'slug','boot','name','Boot','description','','unit_amount','500','currency','USD')));
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,s,rel,1,'refresh',m,sha256(convert_to(m::text,'UTF8')));
 INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(t,s,pg,'home');
 INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES(t,s,pg,'en',1,'{"title":"Home","description":"","blocks":[]}');
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision) VALUES(t,s,rel,'en','/',pg,1);
 -- Test-only: membership as cms_seal_skus would have sealed it at publish.
 INSERT INTO app.site_release_skus(tenant_id,site_id,release_id,product_id,sku_id,ordinal,sku_code,option_values) VALUES(t,s,rel,p,v,0,'BOOT-1','[]');
 EXECUTE 'SET LOCAL ROLE app_domain_verifier';
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES(t,s,'%[5]s','verified',now());
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_publisher';
 PERFORM app.publish_release(t,s,rel,NULL);
 EXECUTE 'RESET ROLE';
END $refresh$`

func loginDSN(t *testing.T, ctx context.Context, admin pgxadapter.Runner, dsn, prefix, role string) string {
	t.Helper()
	name := prefix + strings.ReplaceAll(refreshUUID(t), "-", "")[:12]
	password := strings.ReplaceAll(refreshUUID(t), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"';GRANT "+role+" TO "+quoted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, "DROP ROLE "+quoted); return err })
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(name, password)
	return u.String()
}

func TestServingRefresherProductionSeamPG(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: migrated disposable DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, closeAdmin, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAdmin()
	tenant, site, product, sku := refreshUUID(t), refreshUUID(t), refreshUUID(t), refreshUUID(t)
	host := "refresh-" + site[:8] + ".c15b.test"
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(refreshSiteSQL, tenant, site, product, sku, host))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := pgxadapter.OpenQueryRunner(ctx, loginDSN(t, ctx, admin, dsn, "c15b_runtime_", "app_runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	refresher, err := publish.NewRefresher(publish.RefreshConfig{Run: runner.RunTrustedTx, Tenants: []string{tenant}})
	if err != nil {
		t.Fatal(err)
	}
	public, closePublic, err := publish.Open(ctx, loginDSN(t, ctx, admin, dsn, "c15b_public_", "app_domain_resolver"))
	if err != nil {
		t.Fatal(err)
	}
	defer closePublic()
	skus := func() int {
		t.Helper()
		site, err := public.Current(ctx, host)
		if err != nil || len(site.Products) != 1 || site.TenantID != tenant {
			t.Fatalf("public read: %+v %v", site, err)
		}
		return len(site.Products[0].SKUs)
	}
	if n, err := refresher.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("backfill through RunTrustedTx: %d %v", n, err)
	}
	if skus() != 1 {
		t.Fatal("refreshed serving row has no purchasable SKU")
	}
	// Forced expiry: past valid_until the fallback is served; the refresher restores SKUs.
	expire := func() {
		t.Helper()
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.storefront_serving SET valid_until=now()-interval '1 second' WHERE tenant_id=$1`, tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 2; round++ {
		expire()
		if skus() != 0 {
			t.Fatal("expired row still served its SKUs")
		}
		if n, err := refresher.Sweep(ctx); err != nil || n != 1 {
			t.Fatalf("refresh past expiry (round %d): %d %v", round, n, err)
		}
		if skus() != 1 {
			t.Fatalf("SKUs not purchasable after refresh past expiry (round %d)", round)
		}
	}
}
