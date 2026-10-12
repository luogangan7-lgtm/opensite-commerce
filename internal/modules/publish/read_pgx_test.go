//go:build pgx

// Responsibility: real dedicated-login end-to-end public read/role rejection.
// Dependencies: pgxadapter, migrated expendable DATABASE_URL and C09 SQL fixture.
// Caller: pgx gate; invariant: DATABASE_URL never permits skipped public acceptance.
package publish_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
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
