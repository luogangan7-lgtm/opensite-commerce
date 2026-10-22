//go:build pgx

// Responsibility: storefront serving gates through the real publish/restore path and a
// dedicated public login (ADR_STOREFRONT_SERVING G1-G12).
// Dependencies: disposable migrated PostgreSQL, CMS write harness (newCMSPG), pgxadapter.
// Invariants: serving rows are built in the publish transaction; the public role reads
// only the serving row; membership is fixed at publish.
package publish_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

// servingIDs are shared by both tenants so isolation cannot rely on UUID uniqueness.
// READY sorts first among the product's ready SKUs (cms_products' representative SKU).
type servingIDs struct{ product, ready, archived, unpriced string }

func servingSeed(ids servingIDs, prefix string, price int64, stock int) []string {
	return []string{
		fmt.Sprintf(`INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES($1,'%s','%s-P','ready')`, ids.product, prefix),
		fmt.Sprintf(`INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES
		 ($1,'%[2]s','%[1]s','%[5]s-READY','red','ready'),($1,'%[3]s','%[1]s','%[5]s-ARCHIVED','arch','archived'),($1,'%[4]s','%[1]s','%[5]s-UNPRICED','none','ready')`,
			ids.product, ids.ready, ids.archived, ids.unpriced, prefix),
		// G3 witnesses: 6,000 ready variants with no price must not consume output caps.
		fmt.Sprintf(`INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status)
		 SELECT $1,'%s','%s-U'||g,'u'||g,'ready' FROM generate_series(1,6000) g`, ids.product, prefix),
		fmt.Sprintf(`WITH o AS (INSERT INTO app.product_options(tenant_id,product_id,code) VALUES($1,'%[1]s','color') RETURNING id),
		 v AS (INSERT INTO app.product_option_values(tenant_id,product_id,option_id,value_code,label) SELECT $1,'%[1]s',o.id,'red','%[3]s Red' FROM o RETURNING option_id,id)
		 INSERT INTO app.variant_option_values(tenant_id,product_id,variant_id,option_id,value_id) SELECT $1,'%[1]s','%[2]s',v.option_id,v.id FROM v`, ids.product, ids.ready, prefix),
		fmt.Sprintf(`INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) VALUES($1,'%s','en',1,'Tote','{"description":""}')`, ids.product),
		fmt.Sprintf(`INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) VALUES($1,$2,'%s','en','tote','published',1)`, ids.product),
		// The winning list has infinite bounds; a higher-priority list starts in ten minutes.
		fmt.Sprintf(`WITH l AS (INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,ends_at,priority,audience) VALUES($1,'USD','active','-infinity','infinity',0,'all') RETURNING id)
		 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) SELECT $1,l.id,s,1,%d FROM l,unnest(ARRAY['%s','%s']::uuid[]) s`, price, ids.ready, ids.archived),
		fmt.Sprintf(`WITH l AS (INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority,audience) VALUES($1,'USD','active',now()+interval '10 minutes',9,'all') RETURNING id)
		 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) SELECT $1,l.id,'%s',1,3 FROM l`, ids.ready),
		`SELECT set_config('app.tenant_id',$1::text,true)`,
		fmt.Sprintf(`SELECT app.inventory_adjust('%s',%d,'serving fixture',uuidv7(),'inbound') WHERE $1::uuid IS NOT NULL`, ids.ready, stock),
		fmt.Sprintf(`SELECT app.inventory_adjust('%s',2,'serving fixture',uuidv7(),'inbound') WHERE $1::uuid IS NOT NULL`, ids.archived),
	}
}

func publishHome(t *testing.T, ctx context.Context, f *cmsPG, publication int64, expectedRelease string) (*contract.Release, error) {
	t.Helper()
	created, err := f.exec(t, ctx, contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"Shop","description":""}`)})
	if err != nil {
		t.Fatal(err)
	}
	draft := created.Draft
	if len(draft.Pages) == 0 {
		page, _ := json.Marshal(map[string]any{"page_key": "home", "locale": "en", "path": "/", "document": map[string]any{"title": "Home", "description": "", "blocks": []any{}}, "media_ids": []string{}})
		if _, err = f.exec(t, ctx, contract.Command{Operation: "save_page", DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Payload: page}); err != nil {
			t.Fatal(err)
		}
		draft.Revision++
	}
	r, err := f.exec(t, ctx, contract.Command{Operation: "publish_draft", DraftID: draft.DraftID, ExpectedRevision: draft.Revision, ExpectedReleaseID: expectedRelease, ExpectedPublicationRevision: publication, Payload: []byte(`{"renderer_build_id":"serving"}`)})
	return r.Release, err
}

func verifyHost(t *testing.T, ctx context.Context, f *cmsPG, host string) {
	t.Helper()
	if err := f.admin.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_domain_verifier"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES($1,$2,$3,'verified',now())`, f.tenant, f.site, host)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// publicLogin provisions a dedicated public login (only app_domain_resolver).
func publicLogin(t *testing.T, ctx context.Context, admin pgxadapter.Runner) string {
	t.Helper()
	role := "c15b_public_" + strings.ReplaceAll(newUUID(t), "-", "")[:12]
	password := strings.ReplaceAll(newUUID(t), "-", "")
	quoted := pgx.Identifier{role}.Sanitize()
	if err := admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"';GRANT app_domain_resolver TO "+quoted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.InMaintenance(context.Background(), func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, "DROP ROLE "+quoted); return err })
	})
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

func TestStorefrontServingPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ids := servingIDs{product: newUUID(t), ready: "00000000-0000-4000-8000-" + strings.ReplaceAll(newUUID(t), "-", "")[:12], archived: newUUID(t), unpriced: newUUID(t)}
	const price = 9007199254740993 // > 2^53: must stay an exact decimal string
	a := newCMSPG(t, ctx, servingSeed(ids, "OWN", price, 7331)...)
	b := newCMSPG(t, ctx, servingSeed(ids, "OTHER_TENANT", 4242, 1)...)
	hostA, hostB := "serving-a-"+a.site[:8]+".example.test", "serving-b-"+b.site[:8]+".example.test"
	verifyHost(t, ctx, a, hostA)
	verifyHost(t, ctx, b, hostB)
	dsn := publicLogin(t, ctx, a.admin)
	public, closePublic, err := publish.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closePublic()
	read := func(host string) (contract.PublicSite, []byte) {
		t.Helper()
		body, err := public.Serve(ctx, host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		var site contract.PublicSite
		if err = json.Unmarshal(body, &site); err != nil || len(site.Products) != 1 {
			t.Fatalf("%s: %s %v", host, body, err)
		}
		return site, body
	}
	want := func(prefix, priceMinor string, available bool) []contract.SKU {
		return []contract.SKU{{SKUID: ids.ready, SKUCode: prefix + "-READY", OptionValues: []contract.OptionValue{{Name: "color", Value: prefix + " Red"}}, PriceMinor: priceMinor, Currency: "USD", Available: available}}
	}
	withCMSContextFor(t, true, a.tenant, func(authctx context.Context) {
		r1, err := publishHome(t, authctx, a, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		site, body := read(hostA)
		// G3 + G9: only the priced ready SKU; 6,000 unpriced, archived and unpriced-ready are not members.
		if site.TenantID != a.tenant || site.ReleaseID != r1.ReleaseID || !reflect.DeepEqual(site.Products[0].SKUs, want("OWN", fmt.Sprint(int64(price)), true)) {
			t.Fatalf("served SKUs: %+v", site)
		}
		for _, leak := range []string{"7331", "OTHER_TENANT", "ARCHIVED", "UNPRICED", "on_hand", "reserved", "hostname"} {
			if strings.Contains(string(body), leak) {
				t.Fatalf("public body leaked %q", leak)
			}
		}
		// G2: served bytes are the stored bytes; valid_until is the next price boundary.
		var sha []byte
		var boundary, members int
		if err := a.admin.InTenant(ctx, a.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT body_sha256,(valid_until BETWEEN now()+interval '9 minutes' AND now()+interval '11 minutes')::int,
			 (SELECT count(*) FROM app.site_release_skus WHERE tenant_id=$1 AND release_id=$2) FROM app.storefront_serving WHERE tenant_id=$1`, a.tenant, r1.ReleaseID).Scan(&sha, &boundary, &members)
		}); err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(body); string(sha) != string(sum[:]) || boundary != 1 || members != 1 {
			t.Fatalf("stored bytes/valid_until/membership: sha=%t boundary=%d members=%d", string(sha) == string(sum[:]), boundary, members)
		}
		// G4: the public login cannot read live catalog, pricing, inventory or membership.
		pub, closePub, err := pgxadapter.OpenPublic(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer closePub()
		for _, q := range []string{`SELECT count(*) FROM app.product_variants`, `SELECT count(*) FROM app.price_entries`, `SELECT count(*) FROM app.stock_levels`,
			`SELECT count(*) FROM app.inventory_reservations`, `SELECT count(*) FROM app.site_release_skus`, `SELECT count(serving_generation) FROM app.storefront_serving`} {
			err := pub.InPublicRead(ctx, hostA, func(ctx context.Context, tx pgx.Tx) error {
				var n int
				return tx.QueryRow(ctx, q).Scan(&n)
			})
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("anonymous %s: %v", q, err)
			}
		}
		// G1 + G3 cap: 101 priced SKUs on one product rejects the publish (422) atomically.
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `WITH v AS (INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status)
			 SELECT $1,$2,'CAP-'||g,'cap'||g,'ready' FROM generate_series(1,100) g RETURNING id)
			 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
			 SELECT $1,(SELECT id FROM app.price_lists WHERE tenant_id=$1 AND priority=0),v.id,1,5 FROM v`, a.tenant, ids.product)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before := a.count(t, ctx, `SELECT count(*) FROM app.site_releases WHERE tenant_id=$1 AND site_id=$2`)
		if _, err = publishHome(t, authctx, a, 1, r1.ReleaseID); !errors.Is(err, contract.ErrInvalid) {
			t.Fatalf("cap overrun publish: %v", err)
		}
		if a.count(t, ctx, `SELECT count(*) FROM app.site_releases WHERE tenant_id=$1 AND site_id=$2`) != before ||
			a.count(t, ctx, `SELECT publication_revision::int FROM app.sites WHERE tenant_id=$1 AND id=$2`) != 1 ||
			a.count(t, ctx, `SELECT serving_generation::int FROM app.storefront_serving WHERE tenant_id=$1 AND site_id=$2`) != 1 {
			t.Fatal("failed serving build left a release, pointer move or serving write")
		}
		if again, _ := read(hostA); again.ReleaseID != r1.ReleaseID {
			t.Fatal("failed publish changed the served release")
		}
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.product_variants SET lifecycle_status='archived' WHERE tenant_id=$1 AND sku LIKE 'CAP-%'`, a.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// G7: after valid_until the fallback (products, skus []) is served, not a 503.
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.storefront_serving SET valid_until=now()-interval '1 second' WHERE tenant_id=$1`, a.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if stale, _ := read(hostA); stale.Products[0].SKUs == nil || len(stale.Products[0].SKUs) != 0 || stale.TenantID != a.tenant {
			t.Fatalf("expired serving row: %+v", stale.Products)
		}
		// G10: membership is fixed per release. The archived SKU becomes ready and
		// priced; restoring r1 keeps r1's membership, a new publish adds it.
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.product_variants SET lifecycle_status='ready' WHERE tenant_id=$1 AND id=$2`, a.tenant, ids.archived)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r2, err := publishHome(t, authctx, a, 1, r1.ReleaseID)
		if err != nil {
			t.Fatal(err)
		}
		if site, _ := read(hostA); len(site.Products[0].SKUs) != 2 {
			t.Fatalf("republish did not add the newly purchasable SKU: %+v", site.Products[0].SKUs)
		}
		// G12: two checkouts reserving overlapping member SKUs never wait on serving data.
		// c1 flips READY's availability and stays open; c2 then flips ARCHIVED's and must
		// complete while c1 is open (a shared serving-row lock would block or deadlock it).
		rollback := errors.New("rollback")
		c1Held, c1Done := make(chan struct{}), make(chan struct{})
		c1 := make(chan error, 1)
		go func() {
			c1 <- a.admin.InTenant(ctx, a.tenant, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT app.inventory_reserve_non_order($1::uuid,7331,'checkout',gen_random_uuid(),'g12-c1-'||$2::text,900,uuid_send(gen_random_uuid())||uuid_send(gen_random_uuid()))`, ids.ready, a.tenant); err != nil {
					close(c1Held) // unblock the test so the error surfaces instead of a 10m hang
					return err
				}
				close(c1Held)
				<-c1Done
				return rollback
			})
		}()
		<-c1Held
		c2ctx, c2cancel := context.WithTimeout(ctx, 5*time.Second)
		err = a.admin.InTenant(c2ctx, a.tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT app.inventory_reserve_non_order($1::uuid,2,'checkout',gen_random_uuid(),'g12-c2-'||$2::text,900,uuid_send(gen_random_uuid())||uuid_send(gen_random_uuid()))`, ids.archived, a.tenant); err != nil {
				return err
			}
			return rollback
		})
		c2cancel()
		close(c1Done)
		if !errors.Is(err, rollback) {
			t.Fatalf("overlapping checkout waited on serving data or failed: %v", err)
		}
		if err := <-c1; !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		restored, err := a.exec(t, authctx, contract.Command{Operation: "restore_release", ReleaseID: r1.ReleaseID, ExpectedReleaseID: r2.ReleaseID, ExpectedPublicationRevision: 2})
		if err != nil || restored.Release.PublicationRevision != 3 {
			t.Fatal(restored, err)
		}
		if site, _ := read(hostA); site.ReleaseID != r1.ReleaseID || !reflect.DeepEqual(site.Products[0].SKUs, want("OWN", fmt.Sprint(int64(price)), true)) {
			t.Fatalf("restore did not rebuild from r1's sealed membership: %+v", site)
		}
		// G11: mutations append invalidations in their own transaction; the refresher rebuilds.
		refresh := servingRefresher(t, a)
		mutate := func(sql string, args ...any) {
			t.Helper()
			if err := a.admin.InTenant(ctx, a.tenant, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }); err != nil {
				t.Fatal(err)
			}
		}
		pending := func() int {
			return a.count(t, ctx, `SELECT count(*)::int FROM app.serving_invalidations WHERE tenant_id=$1 AND site_id=$2`)
		}
		sweep := func() {
			t.Helper()
			if _, err := refresh.Sweep(ctx); err != nil || pending() != 0 {
				t.Fatalf("refresh sweep: %v pending=%d", err, pending())
			}
		}
		sweep()
		mutate(`UPDATE app.price_entries SET unit_amount=777 WHERE tenant_id=$1 AND sku_id=$2 AND min_quantity=1 AND unit_amount=$3`, a.tenant, ids.ready, int64(price))
		if pending() == 0 {
			t.Fatal("price change did not append an invalidation")
		}
		sweep()
		if site, _ := read(hostA); !reflect.DeepEqual(site.Products[0].SKUs, want("OWN", "777", true)) {
			t.Fatalf("refresh did not serve the new price: %+v", site.Products[0].SKUs)
		}
		// A reservation that keeps the SKU available appends nothing; one that sells out does.
		mutate(`SELECT app.inventory_reserve_non_order($1::uuid,1,'serving_test',gen_random_uuid(),'serving-a-'||$2::text,900,uuid_send(gen_random_uuid())||uuid_send(gen_random_uuid()))`, ids.ready, a.tenant)
		if pending() != 0 {
			t.Fatal("availability-preserving reservation appended an invalidation")
		}
		mutate(`SELECT app.inventory_reserve_non_order($1::uuid,7330,'serving_test',gen_random_uuid(),'serving-b-'||$2::text,900,uuid_send(gen_random_uuid())||uuid_send(gen_random_uuid()))`, ids.ready, a.tenant)
		if pending() == 0 {
			t.Fatal("sell-out reservation did not append an invalidation")
		}
		sweep()
		if site, _ := read(hostA); !reflect.DeepEqual(site.Products[0].SKUs, want("OWN", "777", false)) {
			t.Fatalf("availability not refreshed: %+v", site.Products[0].SKUs)
		}
		// G5: the refresher blocks on the publication lock BEFORE reading any input.
		locked, release := make(chan struct{}), make(chan struct{})
		holder := make(chan error, 1)
		go func() {
			holder <- a.admin.InTenant(ctx, a.tenant, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM app.sites WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, a.tenant, a.site); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		<-locked
		mutate(`UPDATE app.price_lists SET priority=priority WHERE tenant_id=$1`, a.tenant)
		swept := make(chan error, 1)
		go func() { _, err := refresh.Sweep(ctx); swept <- err }()
		waiting := func(pattern string) int {
			var n int
			if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1`, pattern).Scan(&n)
			}); err != nil {
				t.Fatal(err)
			}
			return n
		}
		deadline := time.Now().Add(15 * time.Second)
		for waiting("%FOR NO KEY UPDATE OF s%") == 0 {
			if time.Now().After(deadline) {
				close(release)
				t.Fatalf("refresh is not waiting on the publication lock (waiting in put: %d)", waiting("%cms_serving_put%"))
			}
			time.Sleep(50 * time.Millisecond)
		}
		if n := waiting("%cms_serving_put%"); n != 0 {
			t.Fatalf("refresh read inputs before the publication lock: %d waiting in put", n)
		}
		close(release)
		if err := <-holder; err != nil {
			t.Fatal(err)
		}
		if err := <-swept; err != nil || pending() != 0 {
			t.Fatalf("refresh after lock release: %v pending=%d", err, pending())
		}
		// Variant archive and product archive each append an invalidation (96-7).
		mutate(`UPDATE app.product_variants SET lifecycle_status='archived' WHERE tenant_id=$1 AND id=$2`, a.tenant, ids.ready)
		if pending() == 0 {
			t.Fatal("variant archive did not append an invalidation")
		}
		sweep()
		if site, _ := read(hostA); len(site.Products[0].SKUs) != 0 {
			t.Fatalf("archived member still served: %+v", site.Products[0].SKUs)
		}
		mutate(`UPDATE app.products SET lifecycle_status='archived' WHERE tenant_id=$1 AND id=$2`, a.tenant, ids.product)
		if pending() == 0 {
			t.Fatal("product archive did not append an invalidation")
		}
		sweep()
	})
	// G8: the other tenant (same product/SKU UUIDs) serves only its own body.
	withCMSContextFor(t, true, b.tenant, func(authctx context.Context) {
		if _, err := publishHome(t, authctx, b, 0, ""); err != nil {
			t.Fatal(err)
		}
	})
	other, body := read(hostB)
	if other.TenantID != b.tenant || !reflect.DeepEqual(other.Products[0].SKUs, want("OTHER_TENANT", "4242", true)) || strings.Contains(string(body), "OWN") {
		t.Fatalf("tenant isolation: %+v", other)
	}
	if mine, _ := read(hostA); mine.TenantID != a.tenant || strings.Contains(fmt.Sprint(mine), "OTHER_TENANT") {
		t.Fatalf("tenant isolation: %+v", mine)
	}
	if _, err := public.Serve(ctx, "unknown-"+a.site[:8]+".example.test"); !errors.Is(err, contract.ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

// servingRefresher runs refresh builds as the runtime role in tenant transactions.
func servingRefresher(t *testing.T, f *cmsPG) *publish.Refresher {
	t.Helper()
	r, err := publish.NewRefresher(publish.RefreshConfig{Tenants: []string{f.tenant}, Run: func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return f.admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// P1 (thread 4260039759): the compatibility read returns tenant and content from one
// snapshot. Moving a hostname between tenants between reads never mixes them, and the
// refresher backfills a serving row for a legacy (SQL-published) site.
func TestStorefrontLegacyHostMovePG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := newCMSPG(t, ctx)
	public, closePublic, err := publish.Open(ctx, publicLogin(t, ctx, a.admin))
	if err != nil {
		t.Fatal(err)
	}
	defer closePublic()
	host := "moved-" + a.site[:8] + ".c09.test"
	t1, t2 := newUUID(t), newUUID(t)
	seed := func(tenant string) {
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(largeSiteSQL, tenant, host, `[]`, 1, 1, "en", 10))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	owner := func(site string) string {
		var tenant string
		if err := a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT tenant_id::text FROM app.sites WHERE id=$1`, site).Scan(&tenant)
		}); err != nil {
			t.Fatal(err)
		}
		return tenant
	}
	seed(t1)
	first, err := public.Current(ctx, host)
	if err != nil || first.TenantID != t1 || owner(first.SiteID) != t1 {
		t.Fatalf("legacy read before move: %+v %v", first, err)
	}
	if err = a.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.site_domains SET status='disabled',verified_at=NULL WHERE tenant_id=$1 AND normalized_hostname=$2`, t1, host)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seed(t2)
	second, err := public.Current(ctx, host)
	if err != nil || second.TenantID != t2 || owner(second.SiteID) != t2 || second.SiteID == first.SiteID {
		t.Fatalf("legacy read after move paired tenant %q with site of %q: %v", second.TenantID, owner(second.SiteID), err)
	}
	// Backfill: the refresher writes a serving row (no SKUs) for the legacy publication.
	b := &cmsPG{admin: a.admin, tenant: t2}
	if n, err := servingRefresher(t, b).Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("legacy backfill: %d %v", n, err)
	}
	if got := b.count(t, ctx, `SELECT count(*) FROM app.storefront_serving WHERE tenant_id=$1 AND $2::text=''`); got != 1 {
		t.Fatalf("backfilled serving rows: %d", got)
	}
	if third, err := public.Current(ctx, host); err != nil || third.TenantID != t2 || third.SiteID != second.SiteID {
		t.Fatalf("served backfill: %+v %v", third, err)
	}
}
