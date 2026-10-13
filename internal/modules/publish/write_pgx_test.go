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
		t.Skip("NOT_RUN: migrated disposable DATABASE_URL required")
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
