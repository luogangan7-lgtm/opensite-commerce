//go:build pgx

// Responsibility: real public-login live SKU price/availability/isolation regression.
// Dependencies: pgxadapter, migrated expendable DATABASE_URL; caller: TestPublicPublishedReadPG.
// Invariant: archived/unpriced SKUs hidden, decimal price strings, boolean-only stock, host isolation.
package publish_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

// skuSiteSQL args: tenant, host, product, sku ready, sku archived, sku unpriced, code prefix, price, stock,
// manifest product id text (tenant A seals it uppercase: UUID text is case-insensitive).
// The same product/SKU UUIDs are reused for two tenants so isolation cannot rely on ID uniqueness.
const skuSiteSQL = `DO $sku$
DECLARE t uuid := '%[1]s'; o uuid := gen_random_uuid(); s uuid := gen_random_uuid(); rel uuid := gen_random_uuid();
 p uuid := '%[3]s'; hidden uuid := gen_random_uuid(); opt uuid := gen_random_uuid(); val uuid := gen_random_uuid();
 lst uuid := gen_random_uuid(); m jsonb;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'SKU '||t);
 PERFORM set_config('app.tenant_id',t::text,true);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,o,'SKU');
 INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(t,s,o,'SKU','en');
 m := jsonb_build_object('version',1,'locale','en','title','SKU','description','','navigation','[]'::jsonb,
  'products',jsonb_build_array(jsonb_build_object('id','%[10]s','slug','tote','name','Tote','description','','unit_amount','1','currency','USD')));
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,s,rel,1,'sku',m,sha256(convert_to(m::text,'UTF8')));
 EXECUTE 'RESET ROLE';
 INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES(t,p,'%[7]s-P','ready'),(t,hidden,'%[7]s-UNLISTED','ready');
 INSERT INTO app.product_options(tenant_id,product_id,id,code) VALUES(t,p,opt,'color');
 INSERT INTO app.product_option_values(tenant_id,product_id,option_id,id,value_code,label) VALUES(t,p,opt,val,'red','%[7]s Red');
 INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES
  (t,'%[4]s',p,'%[7]s-READY','red','ready'),(t,'%[5]s',p,'%[7]s-ARCHIVED','archived','archived'),
  (t,'%[6]s',p,'%[7]s-UNPRICED','none','ready'),(t,gen_random_uuid(),hidden,'%[7]s-UNLISTED-SKU','x','ready');
 INSERT INTO app.variant_option_values(tenant_id,product_id,variant_id,option_id,value_id) VALUES(t,p,'%[4]s',opt,val);
 -- Witnesses: an option used only by the archived SKU and an unassigned option stay hidden.
 INSERT INTO app.product_options(tenant_id,product_id,id,code) VALUES(t,p,gen_random_uuid(),'unassigned');
 WITH o AS (INSERT INTO app.product_options(tenant_id,product_id,id,code) VALUES(t,p,gen_random_uuid(),'size') RETURNING id),
  v AS (INSERT INTO app.product_option_values(tenant_id,product_id,option_id,id,value_code,label) SELECT t,p,o.id,gen_random_uuid(),'xl','ARCHIVED_ONLY' FROM o RETURNING option_id,id)
 INSERT INTO app.variant_option_values(tenant_id,product_id,variant_id,option_id,value_id) SELECT t,p,'%[5]s',v.option_id,v.id FROM v;
 -- The winning list has infinite bounds: valid schema values that must not 503.
 INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at,ends_at,priority,audience) VALUES
  (t,lst,'USD','active','-infinity','infinity',0,'all');
 INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at,priority,audience) VALUES
  (t,gen_random_uuid(),'USD','active',now()+interval '1 day',9,'all'),
  (t,gen_random_uuid(),'USD','draft',now()-interval '1 day',9,'all'),
  (t,gen_random_uuid(),'EUR','active',now()-interval '1 day',9,'all');
 INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at,ends_at,priority,audience) VALUES
  (t,gen_random_uuid(),'USD','active',now()-interval '2 day',now()-interval '1 day',9,'all');
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  SELECT t,l.id,v.id,1,CASE WHEN l.id=lst THEN %[8]d ELSE 1 END FROM app.price_lists l CROSS JOIN app.product_variants v
  WHERE l.tenant_id=t AND v.tenant_id=t AND v.sku<>'%[7]s-UNPRICED';
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES(t,lst,'%[4]s',2,1);
 PERFORM app.inventory_adjust(v.id,%[9]d,'sku fixture',gen_random_uuid(),'inbound') FROM app.product_variants v WHERE v.tenant_id=t;
 EXECUTE 'SET LOCAL ROLE app_domain_verifier';
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES(t,s,'%[2]s','verified',now());
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_publisher';
 PERFORM app.publish_release(t,s,rel,NULL);
 EXECUTE 'RESET ROLE';
END $sku$`

func testLiveSKUs(t *testing.T, ctx context.Context, admin pgxadapter.Runner, module *publish.Module, publicDSN string) {
	product, ready, archived, unpriced := newUUID(t), newUUID(t), newUUID(t), newUUID(t)
	tenantA, tenantB := newUUID(t), newUUID(t)
	hostA, hostB := "sku-a-"+tenantA[:8]+".c09.test", "sku-b-"+tenantB[:8]+".c09.test"
	const price = 9007199254740993 // > 2^53: must survive as an exact decimal string
	for _, f := range []struct {
		tenant, host, prefix, manifestID string
		price, stock                     int64
	}{{tenantA, hostA, "OWN", strings.ToUpper(product), price, 7331}, {tenantB, hostB, "OTHER_TENANT_SKU", product, 4242, 1}} {
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(skuSiteSQL, f.tenant, f.host, product, ready, archived, unpriced, f.prefix, f.price, f.stock, f.manifestID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(host string) (contract.Site, string) {
		t.Helper()
		site, err := module.Current(ctx, host)
		if err != nil || len(site.Products) != 1 {
			t.Fatalf("%s: %+v %v", host, site, err)
		}
		raw, _ := json.Marshal(site)
		return site, string(raw)
	}
	want := func(code, value, priceMinor string, available bool) []contract.SKU {
		return []contract.SKU{{SKUID: ready, SKUCode: code, OptionValues: []contract.OptionValue{{Name: "color", Value: value}}, PriceMinor: priceMinor, Currency: "USD", Available: available}}
	}
	site, raw := read(hostA)
	if site.TenantID != tenantA {
		t.Fatalf("host-resolved tenant id %q, want %q", site.TenantID, tenantA)
	}
	if !reflect.DeepEqual(site.Products[0].SKUs, want("OWN-READY", "OWN Red", "9007199254740993", true)) {
		t.Fatalf("live SKUs (archived/unpriced/unlisted must be hidden): %+v", site.Products[0].SKUs)
	}
	for _, leak := range []string{"7331", "ARCHIVED_ONLY", "unassigned", "OTHER_TENANT_SKU", "ARCHIVED", "UNPRICED", "UNLISTED", "on_hand", "reserved", "quantity"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("public response leaked %q: %s", leak, raw)
		}
	}
	var dto struct {
		Products []struct {
			SKUs []map[string]any `json:"skus"`
		} `json:"products"`
	}
	if err := json.Unmarshal([]byte(raw), &dto); err != nil || len(dto.Products[0].SKUs) != 1 || len(dto.Products[0].SKUs[0]) != 6 {
		t.Fatalf("SKU DTO keys: %+v %v", dto, err)
	}
	other, rawOther := read(hostB)
	if other.TenantID != tenantB || !reflect.DeepEqual(other.Products[0].SKUs, want("OTHER_TENANT_SKU-READY", "OTHER_TENANT_SKU Red", "4242", true)) || strings.Contains(rawOther, "OWN") {
		t.Fatalf("other tenant SKUs: %+v", other.Products[0].SKUs)
	}
	// RLS alone (without the store's explicit filters) scopes the public role to its host.
	r, closeR, err := pgxadapter.OpenPublic(ctx, publicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeR()
	if err = r.InPublicRead(ctx, hostA, func(ctx context.Context, tx pgx.Tx) error {
		var variants, foreign, stock, prices, lists, options, values int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.product_variants),
		 (SELECT count(*) FROM app.product_variants WHERE tenant_id=$1::uuid),
		 (SELECT count(*) FROM app.stock_levels),(SELECT count(*) FROM app.price_entries),(SELECT count(*) FROM app.price_lists),
		 (SELECT count(*) FROM app.product_options),(SELECT count(*) FROM app.product_option_values)`, tenantB).Scan(&variants, &foreign, &stock, &prices, &lists, &options, &values); err != nil {
			return err
		}
		// Only ready SKUs, the one current USD list's quantity-1 row, and assigned options.
		if variants != 2 || foreign != 0 || stock != 2 || prices != 1 || lists != 1 || options != 1 || values != 1 {
			t.Fatalf("public RLS scope: variants=%d foreign=%d stock=%d prices=%d lists=%d options=%d values=%d", variants, foreign, stock, prices, lists, options, values)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = r.InPublicRead(ctx, hostA, func(ctx context.Context, tx pgx.Tx) error {
		var positive bool
		return tx.QueryRow(ctx, `SELECT on_hand>0 FROM app.stock_levels LIMIT 1`).Scan(&positive)
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("stock count readable by public role: %v", err)
	}
	// Reserving the whole stock flips availability live; releasing restores it.
	var reservation string
	stock := func(sql string, args ...any) {
		t.Helper()
		if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenantA); err != nil {
				return err
			}
			return tx.QueryRow(ctx, sql, args...).Scan(&reservation)
		}); err != nil {
			t.Fatal(err)
		}
	}
	stock(`SELECT (app.inventory_reserve($1::uuid,7331,'sku_test',gen_random_uuid(),'sku-test-'||$1)).id::text`, ready)
	if site, _ = read(hostA); !reflect.DeepEqual(site.Products[0].SKUs, want("OWN-READY", "OWN Red", "9007199254740993", false)) {
		t.Fatalf("availability did not flip after reservation: %+v", site.Products[0].SKUs)
	}
	if other, _ = read(hostB); !other.Products[0].SKUs[0].Available {
		t.Fatal("reservation leaked across tenants")
	}
	stock(`SELECT (app.inventory_release($1::uuid)).id::text`, reservation)
	if site, _ = read(hostA); !site.Products[0].SKUs[0].Available {
		t.Fatal("availability did not return after release")
	}
	testManyPriceLists(t, ctx, admin, module)
	// Archiving the live product hides its SKUs while sealed product content remains.
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.products SET lifecycle_status='archived' WHERE tenant_id=$1 AND id=$2", tenantA, product)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if site, _ = read(hostA); site.Products[0].Name != "Tote" || len(site.Products[0].SKUs) != 0 {
		t.Fatalf("archived product SKUs served: %+v", site.Products[0])
	}
}

// manySKUSQL args: tenant, host. 41 released products in five currencies, 4,001 ready SKUs,
// six active audience=all lists (one per currency at priority 5, one extra USD at 1)
// pricing every SKU: 24,006 candidate rows, exactly one winner per SKU in its currency.
const manySKUSQL = `DO $many$
DECLARE t uuid := '%[1]s'; o uuid := gen_random_uuid(); s uuid := gen_random_uuid(); rel uuid := gen_random_uuid(); m jsonb;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'SKU many '||t);
 PERFORM set_config('app.tenant_id',t::text,true);
 CREATE TEMP TABLE many_products ON COMMIT DROP AS SELECT n,gen_random_uuid() AS id FROM generate_series(1,41) n;
 SELECT jsonb_agg(jsonb_build_object('id',id::text,'slug','p'||n,'name','P'||n,'description','','unit_amount','1','currency',(ARRAY['USD','EUR','GBP','JPY','CAD'])[1+n%%5]) ORDER BY n) INTO m FROM many_products;
 m := jsonb_build_object('version',1,'locale','en','title','Many','description','','navigation','[]'::jsonb,'products',m);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,o,'Many');
 INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(t,s,o,'Many','en');
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,s,rel,1,'many',m,sha256(convert_to(m::text,'UTF8')));
 EXECUTE 'RESET ROLE';
 INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) SELECT t,id,'MANY-'||n,'ready' FROM many_products;
 INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status)
  SELECT t,p.id,'S'||g,'k'||g,'ready' FROM generate_series(1,4001) g JOIN many_products p ON p.n=1+(g-1)%%41;
 INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority,audience)
  SELECT t,c,'active',now()-interval '1 day',5,'all' FROM unnest(ARRAY['USD','EUR','GBP','JPY','CAD']) c
  UNION ALL SELECT t,'USD','active',now()-interval '1 day',1,'all';
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  SELECT t,l.id,v.id,1,l.priority FROM app.price_lists l CROSS JOIN app.product_variants v WHERE l.tenant_id=t AND v.tenant_id=t;
 EXECUTE 'SET LOCAL ROLE app_domain_verifier';
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES(t,s,'%[2]s','verified',now());
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_publisher';
 PERFORM app.publish_release(t,s,rel,NULL);
 EXECUTE 'RESET ROLE';
END $many$`

func testManyPriceLists(t *testing.T, ctx context.Context, admin pgxadapter.Runner, module *publish.Module) {
	tenant := newUUID(t)
	host := "sku-many-" + tenant[:8] + ".c09.test"
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(manySKUSQL, tenant, host))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	site, err := module.Current(ctx, host)
	if err != nil {
		t.Fatalf("6 lists in 5 currencies x 4,001 SKUs made the storefront unavailable: %v", err)
	}
	total := 0
	for _, p := range site.Products {
		total += len(p.SKUs)
		for _, s := range p.SKUs {
			if s.PriceMinor != "5" || s.Currency != p.Currency {
				t.Fatalf("highest-priority list in the product currency must win: %+v", s)
			}
		}
	}
	if total != 4001 || site.TenantID != tenant {
		t.Fatalf("served %d SKUs for tenant %q", total, site.TenantID)
	}
}
