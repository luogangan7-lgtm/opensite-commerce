//go:build pgx

// Responsibilities: exercise CMS facade with real controlled SQL and public projection.
// Dependencies: disposable migrated PostgreSQL, pgxadapter and trusted Router.
// Callers: pgx integration gates; invariants: atomic receipts, immutable media/revision
// pins, publication ABA protection and readonly revision-pinned preview grants.
package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCMSWritePublishRestorePG(t *testing.T) {
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
	tenant := newUUID(t)
	site, media, blob, org := newUUID(t), newUUID(t), newUUID(t), newUUID(t)
	host := "cms-" + site + ".example.test"
	// Seed data is namespaced by a fresh UUID in the disposable test database.
	// No existing tenant/customer rows are touched. The parent owns DB teardown.
	err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO app.tenants(id,name) VALUES($1,'C09b service PG')`, []any{tenant}},
			{`INSERT INTO app.organizations(tenant_id,id,display_name) VALUES($1,$2,'C09b')`, []any{tenant, org}},
			{`INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES($1,$2,$3,'C09b','en')`, []any{tenant, site, org}},
			{`INSERT INTO app.media_blobs(tenant_id,id,backend,storage_key,sha256,size_bytes,status) VALUES($1,$2::uuid,'local',$2::uuid::text,sha256(convert_to($2::uuid::text,'UTF8')),0,'ready')`, []any{tenant, blob}},
			{`INSERT INTO app.media_assets(tenant_id,id,blob_id,filename,mime_type,visibility) VALUES($1,$2,$3,'cms.txt','text/plain','public')`, []any{tenant, media, blob}},
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh tenant keeps repeated runs independent within the disposable DB.
	writeRun := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	readRun := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return admin.InTenantSnapshot(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	now := time.Unix(1800000000, 0)
	previewCalls := 0
	m, err := publish.NewWriter(publish.WriteConfig{Run: writeRun, ReadRun: readRun, PreviewKey: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now }, PreviewRun: func(ctx context.Context, c contract.PreviewClaims, fn func(context.Context, pgx.Tx) error) error {
		previewCalls++
		return readRun(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var mode string
			if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&mode); err != nil {
				return err
			}
			if mode != "on" {
				return errors.New("preview is writable")
			}
			return fn(ctx, tx)
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	withCMSContextFor(t, true, tenant, func(authctx context.Context) {
		authctx, cancel := context.WithTimeout(authctx, 60*time.Second)
		defer cancel()
		exec := func(c contract.Command) contract.Result {
			c.TenantID = tenant
			c.SiteID = site
			r, err := m.Execute(authctx, c)
			if err != nil {
				t.Fatalf("%s: %v", c.Operation, err)
			}
			return r
		}
		created := exec(contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"First","description":""}`)})
		if created.Draft == nil || created.Draft.Revision != 1 || created.Draft.PublicationRevision != 0 {
			t.Fatal(created)
		}
		draft := created.Draft.DraftID
		payload := func(key, path, title string, mediaIDs []string) json.RawMessage {
			v := map[string]any{"page_key": key, "locale": "en", "path": path, "document": map[string]any{"title": title, "description": "", "blocks": []any{map[string]any{"kind": "paragraph", "text": "<p>Safe <strong>text</strong></p><script>secret()</script>"}}}, "media_ids": mediaIDs}
			b, _ := json.Marshal(v)
			return b
		}
		home := exec(contract.Command{Operation: "save_page", DraftID: draft, ExpectedRevision: 1, Payload: payload("home", "/", "<b>Home</b>", []string{media})})
		if home.Page == nil || home.Page.Document.Title != "Home" || home.Page.Document.Blocks[0].Text != "Safe text" {
			t.Fatal(home)
		}
		child := exec(contract.Command{Operation: "save_page", DraftID: draft, ExpectedRevision: 2, Payload: payload("guide", "/guides/setup", "Guide", []string{})})
		if child.Page == nil {
			t.Fatal(child)
		}
		nav := []byte(`{"navigation":[{"label":"Home","href":"/"},{"label":"Guide","href":"/guides/setup"}]}`)
		exec(contract.Command{Operation: "set_navigation", DraftID: draft, ExpectedRevision: 3, Payload: nav})
		validated := exec(contract.Command{Operation: "validate_draft", DraftID: draft, ExpectedRevision: 4})
		if validated.Validation == nil || !validated.Validation.Valid {
			t.Fatal(validated)
		}
		grant, err := m.IssuePreview(authctx, contract.Command{TenantID: tenant, SiteID: site, DraftID: draft, ExpectedRevision: 4}, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		preview, err := m.Preview(context.Background(), tenant, site, draft, grant.Token)
		if err != nil || len(preview.Pages) != 2 {
			t.Fatal(preview, err)
		}
		if preview.TenantID != tenant {
			t.Fatalf("preview tenant_id %q, want %q", preview.TenantID, tenant)
		}
		beforeCalls := previewCalls
		for _, tok := range []string{grant.Token + "x", "bad"} {
			if _, err = m.Preview(context.Background(), tenant, site, draft, tok); !errors.Is(err, contract.ErrPreviewDenied) {
				t.Fatal("forged token", err)
			}
		}
		if _, err = m.Preview(context.Background(), cmsSite, site, draft, grant.Token); !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("cross tenant", err)
		}
		now = grant.ExpiresAt
		if _, err = m.Preview(context.Background(), tenant, site, draft, grant.Token); !errors.Is(err, contract.ErrPreviewDenied) {
			t.Fatal("expired", err)
		}
		now = now.Add(-5 * time.Minute)
		if previewCalls != beforeCalls {
			t.Fatal("invalid token reached DB")
		}
		receiptErr := errors.New("C04 receipt storage failed")
		_, err = m.Execute(authctx, contract.Command{TenantID: tenant, SiteID: site, Operation: "set_metadata", DraftID: draft, ExpectedRevision: 4, Payload: []byte(`{"locale":"en","title":"Rollback","description":""}`), Complete: func(context.Context, pgx.Tx, contract.Result) error { return receiptErr }})
		if !errors.Is(err, receiptErr) {
			t.Fatal(err)
		}
		after := exec(contract.Command{Operation: "get_draft", DraftID: draft})
		if after.Draft.Revision != 4 || after.Draft.Title != "First" {
			t.Fatal("receipt failure committed mutation", after)
		}
		del, _ := json.Marshal(map[string]string{"id": child.Page.ID, "locale": "en"})
		_, err = m.Execute(authctx, contract.Command{TenantID: tenant, SiteID: site, Operation: "delete_page", DraftID: draft, ExpectedRevision: 4, Payload: del})
		if !errors.Is(err, contract.ErrInvalid) {
			t.Fatal("deletion left dangling navigation", err)
		}
		after = exec(contract.Command{Operation: "get_draft", DraftID: draft})
		if after.Draft.Revision != 4 || len(after.Draft.Pages) != 2 {
			t.Fatal("invalid deletion changed draft", after)
		}
		remove, _ := json.Marshal(map[string]any{"operations": []any{map[string]any{"operation": "delete_page", "payload": json.RawMessage(del)}, map[string]any{"operation": "set_navigation", "payload": map[string]any{"navigation": []any{}}}}})
		exec(contract.Command{Operation: "apply_operations", DraftID: draft, ExpectedRevision: 4, Payload: remove})
		savedPayload := payload("guide", "/guides/setup", "Guide", []string{})
		var saved map[string]any
		_ = json.Unmarshal(savedPayload, &saved)
		saved["id"] = child.Page.ID
		deletedUpdate, _ := json.Marshal(saved)
		_, err = m.Execute(authctx, contract.Command{TenantID: tenant, SiteID: site, Operation: "update_page", DraftID: draft, PageID: child.Page.ID, ExpectedRevision: 5, Payload: deletedUpdate})
		if !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("update resurrected deleted page", err)
		}
		batch, _ := json.Marshal(map[string]any{"operations": []any{map[string]any{"operation": "save_page", "payload": saved}, map[string]any{"operation": "set_navigation", "payload": json.RawMessage(nav)}}})
		batchResult := exec(contract.Command{Operation: "apply_operations", DraftID: draft, ExpectedRevision: 5, Payload: batch})
		if batchResult.Draft.Revision != 6 {
			t.Fatal("batch revision", batchResult)
		}
		if _, err = m.Preview(context.Background(), tenant, site, draft, grant.Token); !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("stale preview", err)
		}
		a := exec(contract.Command{Operation: "publish_draft", DraftID: draft, ExpectedRevision: 6, ExpectedPublicationRevision: 0, Payload: []byte(`{"renderer_build_id":"c09b-test"}`)}).Release
		if a == nil || a.PublicationRevision != 1 || len(a.ManifestHash) != 64 {
			t.Fatal(a)
		}
		// Domain trust is granted separately after publishing, through the verifier.
		if err = admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_domain_verifier"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES($1,$2,$3,'verified',now())`, tenant, site, host)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		role := "c09b_public_" + strings.ReplaceAll(site, "-", "")[:12]
		password := strings.ReplaceAll(newUUID(t), "-", "")
		quoted := pgx.Identifier{role}.Sanitize()
		if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"';GRANT app_domain_resolver TO "+quoted)
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
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, password)
		public, closePublic, err := publish.Open(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer closePublic()
		current, err := public.Current(ctx, host)
		if err != nil || current.ReleaseID != a.ReleaseID || len(current.Pages) != 2 {
			t.Fatal(current, err)
		}
		clone := exec(contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"Second","description":""}`)}).Draft
		if clone.BaseReleaseID == nil || *clone.BaseReleaseID != a.ReleaseID || len(clone.Pages) != 2 {
			t.Fatal(clone)
		}
		update := payload("home", "/", "New Home", []string{media})
		var updateMap map[string]any
		_ = json.Unmarshal(update, &updateMap)
		updateMap["id"] = home.Page.ID
		update, _ = json.Marshal(updateMap)
		exec(contract.Command{Operation: "update_page", DraftID: clone.DraftID, PageID: home.Page.ID, ExpectedRevision: 1, Payload: update})
		b := exec(contract.Command{Operation: "publish_draft", DraftID: clone.DraftID, ExpectedRevision: 2, ExpectedReleaseID: a.ReleaseID, ExpectedPublicationRevision: 1, Payload: []byte(`{"renderer_build_id":"c09b-test"}`)}).Release
		current, err = public.Current(ctx, host)
		if err != nil || current.ReleaseID != b.ReleaseID || current.Title != "Second" {
			t.Fatal(current, err)
		}
		restored := exec(contract.Command{Operation: "restore_release", ReleaseID: a.ReleaseID, ExpectedReleaseID: b.ReleaseID, ExpectedPublicationRevision: 2}).Release
		if restored.PublicationRevision != 3 {
			t.Fatal(restored)
		}
		current, err = public.Current(ctx, host)
		if err != nil || current.ReleaseID != a.ReleaseID || current.Title != "First" {
			t.Fatal(current, err)
		}
		_, err = m.Execute(authctx, contract.Command{TenantID: tenant, SiteID: site, Operation: "restore_release", ReleaseID: b.ReleaseID, ExpectedReleaseID: a.ReleaseID, ExpectedPublicationRevision: 1})
		if !errors.Is(err, contract.ErrConflict) {
			t.Fatal("ABA accepted", err)
		}
		releases := exec(contract.Command{Operation: "list_releases"})
		if len(releases.Releases) != 2 {
			t.Fatal(releases)
		}
		var pins, operations, audits int
		err = admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.site_release_media WHERE tenant_id=$1 AND site_id=$2 AND release_id=$3 AND media_id=$4),(SELECT count(*) FROM app.draft_operations WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$5),(SELECT count(*) FROM app.audit_events WHERE tenant_id=$1 AND operation LIKE 'cms.%')`, tenant, site, a.ReleaseID, media, draft).Scan(&pins, &operations, &audits)
		})
		if err != nil || pins != 1 || operations != 5 || audits < 10 {
			t.Fatal(fmt.Sprintf("pins%d ops%d audits%d", pins, operations, audits), err)
		}
		// Legacy data can predate canonical locale admission. Restore must reuse
		// the public renderer before its pointer, audit and receipt can commit.
		legacySite, legacyPage, legacyRelease := newUUID(t), newUUID(t), newUUID(t)
		badLocale := "en-abcde-abcde"
		err = admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			statements := []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES($1,$2,$3,'Legacy CMS',$4)`, []any{tenant, legacySite, org, badLocale}},
				{`INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES($1,$2,$3,'home')`, []any{tenant, legacySite, legacyPage}},
				{`INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES($1,$2,$3,$4,1,'{"title":"Legacy","description":"","blocks":[]}'::jsonb)`, []any{tenant, legacySite, legacyPage, badLocale}},
				{`INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) VALUES($1,$2,$3,1,'legacy',jsonb_build_object('version',1,'locale',$4::text,'title','Legacy','description','','navigation','[]'::jsonb,'products','[]'::jsonb),sha256(convert_to('legacy','UTF8')))`, []any{tenant, legacySite, legacyRelease, badLocale}},
				{`INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision) VALUES($1,$2,$3,$4,'/',$5,1)`, []any{tenant, legacySite, legacyRelease, badLocale, legacyPage}},
				{`UPDATE app.site_releases SET sealed=true WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, []any{tenant, legacySite, legacyRelease}},
			}
			for _, statement := range statements {
				if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		completeCalls := 0
		_, err = m.Execute(authctx, contract.Command{TenantID: tenant, SiteID: legacySite, Operation: "restore_release", ReleaseID: legacyRelease, ExpectedPublicationRevision: 0, Complete: func(context.Context, pgx.Tx, contract.Result) error { completeCalls++; return nil }})
		if !errors.Is(err, contract.ErrInvalid) || completeCalls != 0 {
			t.Fatal("invalid legacy projection committed", err, completeCalls)
		}
		var legacyEpoch, legacyAudits int
		var unpublished bool
		err = admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT publication_revision,published_release_id IS NULL,(SELECT count(*) FROM app.audit_events WHERE tenant_id=$1 AND resource_id=$2) FROM app.sites WHERE tenant_id=$1 AND id=$2`, tenant, legacySite).Scan(&legacyEpoch, &unpublished, &legacyAudits)
		})
		if err != nil || legacyEpoch != 0 || !unpublished || legacyAudits != 0 {
			t.Fatal("invalid legacy restore did not roll back", legacyEpoch, unpublished, legacyAudits, err)
		}
	})
}

// publishHookTx runs before once, immediately before the cms_publish statement,
// to pin a deterministic READ COMMITTED interleaving inside the facade's TX.
type publishHookTx struct {
	pgx.Tx
	before func()
}

func (h publishHookTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if h.before != nil && strings.Contains(sql, "cms_publish(") {
		h.before()
	}
	return h.Tx.QueryRow(ctx, sql, args...)
}

type cmsPG struct {
	admin        pgxadapter.Runner
	m            *publish.WriteModule
	tenant, site string
	beforeSeal   func()
}

// newCMSPG seeds a fresh tenant/site plus extra maintenance SQL ($1 tenant, $2 site).
func newCMSPG(t *testing.T, ctx context.Context, seed ...string) *cmsPG {
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
	f := &cmsPG{admin: admin, tenant: newUUID(t), site: newUUID(t)}
	org := newUUID(t)
	err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		statements := append([]string{
			`INSERT INTO app.tenants(id,name) VALUES($1,'C09b bounds PG')`,
			`INSERT INTO app.organizations(tenant_id,id,display_name) SELECT $1,$3,'C09b' WHERE $2::uuid IS NOT NULL`,
			`INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES($1,$2,$3,'C09b','en')`,
		}, seed...)
		for _, statement := range statements {
			args := []any{f.tenant, f.site, org}
			for !strings.Contains(statement, fmt.Sprintf("$%d", len(args))) {
				args = args[:len(args)-1]
			}
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	role := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime")
		return err
	}
	writeRun := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return admin.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := role(ctx, tx); err != nil {
				return err
			}
			return fn(ctx, publishHookTx{Tx: tx, before: func() {
				if f.beforeSeal != nil {
					hook := f.beforeSeal
					f.beforeSeal = nil
					hook()
				}
			}})
		})
	}
	readRun := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return admin.InTenantSnapshot(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := role(ctx, tx); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	f.m, err = publish.NewWriter(publish.WriteConfig{Run: writeRun, ReadRun: readRun, PreviewKey: []byte(strings.Repeat("k", 32)), PreviewRun: func(context.Context, contract.PreviewClaims, func(context.Context, pgx.Tx) error) error {
		return errors.New("unused")
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cmsPG) exec(t *testing.T, ctx context.Context, c contract.Command) (contract.Result, error) {
	c.TenantID, c.SiteID = f.tenant, f.site
	return f.m.Execute(ctx, c)
}

func (f *cmsPG) count(t *testing.T, ctx context.Context, sql string) int {
	t.Helper()
	var n int
	if err := f.admin.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, f.tenant, f.site).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// Regression for the page x release media cross product: 100 pages x 10
// distinct pins used to clone into 100,000 rows and a ~4 MB (503) draft read.
func TestCMSDraftCloneKeepsPerPageMediaPG(t *testing.T) {
	const pages, perPage = 100, 10
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newCMSPG(t, ctx,
		`INSERT INTO app.media_blobs(tenant_id,id,backend,storage_key,sha256,size_bytes,status) SELECT $1,b,'local',b::text,sha256(convert_to(b::text,'UTF8')),0,'ready' FROM (SELECT uuidv7() b FROM generate_series(1,1000)) x`,
		`INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility) SELECT $1,id,'cms.txt','text/plain','public' FROM app.media_blobs WHERE tenant_id=$1`)
	var media []string
	if err := f.admin.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY id) FROM app.media_assets WHERE tenant_id=$1`, f.tenant).Scan(&media)
	}); err != nil || len(media) != pages*perPage {
		t.Fatal(len(media), err)
	}
	withCMSContextFor(t, true, f.tenant, func(authctx context.Context) {
		created, err := f.exec(t, authctx, contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"Bounds","description":""}`)})
		if err != nil {
			t.Fatal(err)
		}
		ops := make([]any, 0, pages)
		for i := 0; i < pages; i++ {
			path := fmt.Sprintf("/p%d", i)
			if i == 0 {
				path = "/"
			}
			ops = append(ops, map[string]any{"operation": "save_page", "payload": map[string]any{"page_key": fmt.Sprintf("p%d", i), "locale": "en", "path": path,
				"document": map[string]any{"title": "Page", "description": "", "blocks": []any{}}, "media_ids": media[i*perPage : (i+1)*perPage]}})
		}
		batch, _ := json.Marshal(map[string]any{"operations": ops})
		draft := created.Draft.DraftID
		if _, err = f.exec(t, authctx, contract.Command{Operation: "apply_operations", DraftID: draft, ExpectedRevision: 1, Payload: batch}); err != nil {
			t.Fatal(err)
		}
		if _, err = f.exec(t, authctx, contract.Command{Operation: "publish_draft", DraftID: draft, ExpectedRevision: 2, Payload: []byte(`{"renderer_build_id":"bounds"}`)}); err != nil {
			t.Fatal(err)
		}
		clone, err := f.exec(t, authctx, contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"Bounds 2","description":""}`)})
		if err != nil {
			t.Fatal("clone of a valid release failed:", err)
		}
		body, _ := json.Marshal(clone.Draft)
		// Exactly the published pins, each on its own page: ~45 KB of UUIDs.
		if len(clone.Draft.Pages) != pages || len(body) > 128<<10 {
			t.Fatalf("pages%d bytes%d", len(clone.Draft.Pages), len(body))
		}
		owner := map[string]string{}
		for i, id := range media {
			owner[id] = fmt.Sprintf("p%d", i/perPage)
		}
		for _, p := range clone.Draft.Pages {
			if len(p.MediaIDs) != perPage {
				t.Fatalf("%s has %d pins", p.PageKey, len(p.MediaIDs))
			}
			for _, id := range p.MediaIDs {
				if owner[id] != p.PageKey {
					t.Fatalf("%s carries %s's pin", p.PageKey, owner[id])
				}
			}
		}
		rows := f.count(t, ctx, `SELECT count(*) FROM app.draft_page_media WHERE tenant_id=$1 AND site_id=$2 AND draft_id='`+clone.Draft.DraftID+`'`)
		if rows != pages*perPage {
			t.Fatalf("draft_page_media rows %d, want %d", rows, pages*perPage)
		}
	})
}

// A catalog writer committing between draft validation and sealing must not
// publish an unrenderable release: Project runs on the sealed snapshot in-TX.
func TestCMSPublishValidatesSealedSnapshotPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// 20 priced products; content revision 2 has 10,000 '&' (each JSON-escaped
	// to &), passing every SQL bound but exceeding the 1 MiB projection.
	f := newCMSPG(t, ctx,
		`INSERT INTO app.products(tenant_id,product_code,lifecycle_status) SELECT $1,'amp-'||g,'ready' FROM generate_series(1,20) g`,
		`INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status) SELECT tenant_id,id,product_code,'default','ready' FROM app.products WHERE tenant_id=$1`,
		`INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority) SELECT $1,'USD','active','2026-01-01Z',1`,
		`INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) SELECT $1,l.id,v.id,1,100 FROM app.price_lists l JOIN app.product_variants v ON v.tenant_id=l.tenant_id WHERE l.tenant_id=$1`,
		`INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) SELECT $1,id,'en',r,'Amp',jsonb_build_object('description',CASE r WHEN 1 THEN 'short' ELSE repeat('&',10000) END) FROM app.products, generate_series(1,2) r WHERE tenant_id=$1`,
		`INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) SELECT $1,$2,id,'en',product_code,'published',1 FROM app.products WHERE tenant_id=$1`)
	withCMSContextFor(t, true, f.tenant, func(authctx context.Context) {
		created, err := f.exec(t, authctx, contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en","title":"Seal","description":""}`)})
		if err != nil {
			t.Fatal(err)
		}
		draft := created.Draft.DraftID
		home := []byte(`{"page_key":"home","locale":"en","path":"/","document":{"title":"Home","description":"","blocks":[]},"media_ids":[]}`)
		if _, err = f.exec(t, authctx, contract.Command{Operation: "save_page", DraftID: draft, ExpectedRevision: 1, Payload: home}); err != nil {
			t.Fatal(err)
		}
		f.beforeSeal = func() {
			if err := f.admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE app.product_listings SET published_content_revision=2 WHERE tenant_id=$1`, f.tenant)
				return err
			}); err != nil {
				t.Error(err)
			}
		}
		completed := 0
		_, err = f.exec(t, authctx, contract.Command{Operation: "publish_draft", DraftID: draft, ExpectedRevision: 2, Payload: []byte(`{"renderer_build_id":"seal"}`),
			Complete: func(context.Context, pgx.Tx, contract.Result) error { completed++; return nil }})
		if f.beforeSeal != nil {
			t.Fatal("interleaving hook did not run")
		}
		if !errors.Is(err, contract.ErrInvalid) || completed != 0 {
			t.Fatal("unrenderable sealed release committed:", err, completed)
		}
		state := f.count(t, ctx, `SELECT (s.publication_revision=0 AND s.published_release_id IS NULL AND d.status='open')::int+(SELECT count(*) FROM app.site_releases WHERE tenant_id=$1 AND site_id=$2)+(SELECT count(*) FROM app.audit_events WHERE tenant_id=$1 AND operation='cms.publish')
			FROM app.sites s JOIN app.site_drafts d ON (d.tenant_id,d.site_id)=(s.tenant_id,s.id) WHERE s.tenant_id=$1 AND s.id=$2`)
		if state != 1 {
			t.Fatal("pointer/draft/release/audit not rolled back", state)
		}
	})
}

// Review regressions: legacy locale casing, mixed-currency restore, page_key
// recreation and bounded release history, through the real facade and SQL.
func TestCMSReviewRegressionsPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newCMSPG(t, ctx,
		`UPDATE app.sites SET default_locale='en-us' WHERE tenant_id=$1 AND id=$2`,
		`INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES($1,'usd','ready'),($1,'eur','ready')`,
		`INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status) SELECT tenant_id,id,product_code,'default','ready' FROM app.products WHERE tenant_id=$1`,
		`INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,priority) VALUES($1,'USD','active','2026-01-01Z',1),($1,'EUR','active','2026-01-01Z',1)`,
		`INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) SELECT $1,l.id,v.id,1,100 FROM app.price_lists l JOIN app.product_variants v ON v.tenant_id=l.tenant_id AND lower(v.sku)=lower(l.currency) WHERE l.tenant_id=$1`,
		`INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) SELECT $1,id,'en-us',1,upper(product_code),'{}' FROM app.products WHERE tenant_id=$1`,
		`INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) SELECT $1,$2,id,'en-us',product_code,'published',1 FROM app.products WHERE tenant_id=$1`)
	withCMSContextFor(t, true, f.tenant, func(authctx context.Context) {
		must := func(c contract.Command) contract.Result {
			t.Helper()
			r, err := f.exec(t, authctx, c)
			if err != nil {
				t.Fatalf("%s: %v", c.Operation, err)
			}
			return r
		}
		page := func(key, path string, id string) json.RawMessage {
			v := map[string]any{"page_key": key, "locale": "en-US", "path": path, "document": map[string]any{"title": key, "description": "", "blocks": []any{}}, "media_ids": []string{}}
			if id != "" {
				v["id"] = id
			}
			b, _ := json.Marshal(v)
			return b
		}
		// Legacy en-us site: create/edit/publish with canonical en-US input.
		d := must(contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en-us","title":"Case","description":""}`)}).Draft
		if len(d.Products) != 2 {
			t.Fatalf("case-insensitive listings %d", len(d.Products))
		}
		must(contract.Command{Operation: "save_page", DraftID: d.DraftID, ExpectedRevision: 1, Payload: page("home", "/", "")})
		guide := must(contract.Command{Operation: "save_page", DraftID: d.DraftID, ExpectedRevision: 2, Payload: page("guide", "/guide", "")}).Page
		del, _ := json.Marshal(map[string]string{"id": guide.ID, "locale": "en-US"})
		must(contract.Command{Operation: "delete_page", DraftID: d.DraftID, ExpectedRevision: 3, Payload: del})
		again := must(contract.Command{Operation: "save_page", DraftID: d.DraftID, ExpectedRevision: 4, Payload: page("guide", "/guide", "")}).Page
		if again.ID != guide.ID {
			t.Fatal("recreated page_key did not reattach its identity", again.ID, guide.ID)
		}
		if _, err := f.exec(t, authctx, contract.Command{Operation: "save_page", DraftID: d.DraftID, ExpectedRevision: 5, Payload: page("guide", "/other", "")}); !errors.Is(err, contract.ErrConflict) {
			t.Fatal("duplicate page_key in one draft", err)
		}
		r1 := must(contract.Command{Operation: "publish_draft", DraftID: d.DraftID, ExpectedRevision: 5, Payload: []byte(`{"renderer_build_id":"r1"}`)}).Release
		clone := must(contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en-US","title":"Case 2","description":""}`)}).Draft
		if len(clone.Pages) != 2 {
			t.Fatalf("clone pages %d", len(clone.Pages))
		}
		r2 := must(contract.Command{Operation: "publish_draft", DraftID: clone.DraftID, ExpectedRevision: 1, ExpectedReleaseID: r1.ReleaseID, ExpectedPublicationRevision: 1, Payload: []byte(`{"renderer_build_id":"r2"}`)}).Release
		// R1 seals USD and EUR products; restoring it must not add a rule publish lacks.
		restored := must(contract.Command{Operation: "restore_release", ReleaseID: r1.ReleaseID, ExpectedReleaseID: r2.ReleaseID, ExpectedPublicationRevision: 2}).Release
		if restored.PublicationRevision != 3 {
			t.Fatal(restored)
		}
		// History stays bounded to the 100 most recent releases.
		current, epoch := r1.ReleaseID, int64(3)
		for i := 0; i < 100; i++ {
			c := must(contract.Command{Operation: "create_draft", Payload: []byte(`{"locale":"en-US","title":"Many","description":""}`)}).Draft
			current = must(contract.Command{Operation: "publish_draft", DraftID: c.DraftID, ExpectedRevision: 1, ExpectedReleaseID: current, ExpectedPublicationRevision: epoch, Payload: []byte(`{"renderer_build_id":"many"}`)}).Release.ReleaseID
			epoch++
		}
		history := must(contract.Command{Operation: "list_releases"})
		if len(history.Releases) != 100 || history.Releases[0].ReleaseID != current || history.PublicationRevision != epoch {
			t.Fatalf("history %d first=%s epoch=%d", len(history.Releases), history.Releases[0].ReleaseID, history.PublicationRevision)
		}
	})
}
