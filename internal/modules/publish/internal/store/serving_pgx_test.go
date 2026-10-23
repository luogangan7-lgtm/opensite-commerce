//go:build pgx

// Responsibility: statement-boundary gates for the serving store (ADR G5 folded set, G8
// single snapshot), driven through test-only seams instead of sleeps.
// Dependencies: disposable migrated DATABASE_URL, pgxadapter; caller: pgx CI gate.
package store

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
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func storeUUID(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// legacySiteSQL publishes one sealed release through app.publish_release (no serving row).
// Args: tenant, site, verified host.
const legacySiteSQL = `DO $legacy$
DECLARE t uuid := '%[1]s'; o uuid := gen_random_uuid(); s uuid := '%[2]s'; pg uuid := gen_random_uuid(); rel uuid := gen_random_uuid(); m jsonb;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'serving store '||t);
 PERFORM set_config('app.tenant_id',t::text,true);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,o,'Serving');
 INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(t,s,o,'Serving','en');
 m := jsonb_build_object('version',1,'locale','en','title','Serving '||t,'description','','navigation','[]'::jsonb,'products','[]'::jsonb);
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,s,rel,1,'serving-store',m,sha256(convert_to(m::text,'UTF8')));
 INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(t,s,pg,'about');
 INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document)
  VALUES(t,s,pg,'en',1,'{"title":"About","description":"","blocks":[]}');
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision) VALUES(t,s,rel,'en','/about',pg,1);
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_domain_verifier';
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES(t,s,'%[3]s','verified',now());
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_publisher';
 PERFORM app.publish_release(t,s,rel,NULL);
 EXECUTE 'RESET ROLE';
END $legacy$`

type storeFixture struct {
	admin pgxadapter.Runner
	dsn   string
}

func newStoreFixture(t *testing.T, ctx context.Context) storeFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: migrated disposable DATABASE_URL required")
	}
	admin, closeAdmin, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAdmin)
	return storeFixture{admin: admin, dsn: dsn}
}

func (f storeFixture) exec(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if err := f.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func (f storeFixture) runtime(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	return f.admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func (f storeFixture) publicStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	role := "c15b_store_" + strings.ReplaceAll(storeUUID(t), "-", "")[:12]
	password := strings.ReplaceAll(storeUUID(t), "-", "")
	pgxtest.NewRole(t, ctx, f.dsn, role, password, true, "app_domain_resolver")
	u, err := url.Parse(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	s, closeStore, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStore)
	return s
}

// G8: the hostname moves to another tenant's site BETWEEN the serving lookup and the
// legacy snapshot read. Tenant and content must still come from one snapshot (both the
// old owner); taking the tenant from one read and content from another would mix them.
func TestServingLegacySingleSnapshotPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStoreFixture(t, ctx)
	public := f.publicStore(t, ctx)
	t1, s1, t2, s2 := storeUUID(t), storeUUID(t), storeUUID(t), storeUUID(t)
	host := "snapshot-" + s1[:8] + ".c15b.test"
	f.exec(t, ctx, fmt.Sprintf(legacySiteSQL, t1, s1, host))
	moved := false
	afterServingLookup = func() {
		if moved {
			return
		}
		moved = true
		f.exec(t, ctx, `UPDATE app.site_domains SET status='disabled',verified_at=NULL WHERE tenant_id=$1 AND normalized_hostname=$2`, t1, host)
		f.exec(t, ctx, fmt.Sprintf(legacySiteSQL, t2, s2, host))
	}
	t.Cleanup(func() { afterServingLookup = nil })
	body, legacy, err := public.Serving(ctx, host)
	if err != nil || body != nil || legacy == nil || !moved {
		t.Fatalf("legacy read: body=%v legacy=%+v moved=%v err=%v", body != nil, legacy, moved, err)
	}
	if legacy.TenantID != t1 || legacy.SiteID != s1 {
		t.Fatalf("tenant %s paired with site %s (expected the pre-move snapshot %s/%s)", legacy.TenantID, legacy.SiteID, t1, s1)
	}
	afterServingLookup = nil
	if _, after, err := public.Serving(ctx, host); err != nil || after == nil || after.TenantID != t2 || after.SiteID != s2 {
		t.Fatalf("read after move: %+v %v", after, err)
	}
}

// G5: an invalidation committed after the refresher read its folded set (while it builds)
// is not deleted: the row stays dirty and the next sweep rebuilds it.
func TestServingFoldedSetKeepsLateInvalidationPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStoreFixture(t, ctx)
	tenant, site := storeUUID(t), storeUUID(t)
	f.exec(t, ctx, fmt.Sprintf(legacySiteSQL, tenant, site, "folded-"+site[:8]+".c15b.test"))
	if n, err := RefreshDue(ctx, f.runtime, tenant); err != nil || n != 1 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	count := func() (n int) {
		t.Helper()
		if err := f.admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	invalidate := func() {
		t.Helper()
		if err := f.admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority,audience) VALUES($1,'USD','draft',now(),0,'all')`, tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	invalidate()
	if count() != 1 {
		t.Fatal("price list change did not append an invalidation")
	}
	late := false
	afterServingLock = func() {
		if !late {
			late = true
			invalidate() // commits while the refresher holds its folded set
		}
	}
	t.Cleanup(func() { afterServingLock = nil })
	if n, err := RefreshDue(ctx, f.runtime, tenant); err != nil || n != 1 || !late {
		t.Fatalf("refresh with late invalidation: %d %v", n, err)
	}
	if got := count(); got != 1 {
		t.Fatalf("folded/late invalidations after refresh: %d, want exactly the late one", got)
	}
	afterServingLock = nil
	if n, err := RefreshDue(ctx, f.runtime, tenant); err != nil || n != 1 || count() != 0 {
		t.Fatalf("late invalidation not refreshed: %d %v remaining=%d", n, err, count())
	}
}
