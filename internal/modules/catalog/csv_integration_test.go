//go:build pgx

// Responsibilities: actual CSV SQL, rollback, revisions and tenant isolation.
// Dependencies: public pgxadapter runners, dedicated disposable C22a PG18 fixture.
// Callers: C22a focused/full gates. Invariants: no raw connection or real data.
package catalog

import (
	"context"
	"encoding/csv"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func csvPGFixture(t *testing.T) (pgxadapter.Runner, string, string) {
	t.Helper()
	runtimeDSN := os.Getenv("C22A_DATABASE_URL")
	adminDSN := os.Getenv("C22A_MAINTENANCE_DATABASE_URL")
	if runtimeDSN == "" || adminDSN == "" {
		pgxtest.Skip(t, "NOT_RUN: dedicated disposable C22A_DATABASE_URL + C22A_MAINTENANCE_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, closeAdmin, err := pgxadapter.Open(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAdmin()
	a, b := uuid.NewString(), uuid.NewString()
	err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.tenants(id,name) VALUES($1::uuid,'CSV synthetic A'),($2::uuid,'CSV synthetic B')`, a, b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(runtimeDSN)
	if err != nil {
		t.Fatal("invalid dedicated runtime DSN")
	}
	query := parsed.Query()
	query.Set("options", strings.TrimSpace(query.Get("options")+" -c role=app_runtime"))
	parsed.RawQuery = query.Encode()
	runner, closeRuntime, err := pgxadapter.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	return runner, a, b
}
func csvPGInput(records ...[]string) string {
	var out strings.Builder
	writer := csv.NewWriter(&out)
	_ = writer.Write(csvHeader)
	for _, r := range records {
		_ = writer.Write(r)
	}
	writer.Flush()
	return out.String()
}
func TestPGCSVAtomicRoundTripAndIsolation(t *testing.T) {
	runner, a, b := csvPGFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port := NewCSV()
	run := func(tenant string, fn func(context.Context, pgx.Tx) error) {
		t.Helper()
		if err := runner.InTenant(ctx, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"P1", "=SUM(1,2)\n中文", "S1", "en"}, []string{"P1", "中文标题", "S1", "zh-cn"}, []string{"P2", "Product only", "", "en"})))
		if report.Imported != 3 {
			t.Fatalf("%+v", report)
		}
		return err
	})
	run(b, func(ctx context.Context, tx pgx.Tx) error {
		_, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"P1", "OTHER TENANT SECRET SYNTHETIC", "S1", "en"})))
		return err
	})
	var exported []byte
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exported, err = port.Export(ctx, tx)
		return err
	})
	if strings.Contains(string(exported), "OTHER TENANT") || !strings.Contains(string(exported), "'=SUM") {
		t.Fatalf("RLS or escaping failed: %s", exported)
	}
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(string(exported)))
		if report.Imported != 3 {
			t.Fatalf("%+v", report)
		}
		return err
	})
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM app.product_content_revisions WHERE tenant_id=$1::uuid`, a).Scan(&n)
		if n != 3 {
			t.Fatalf("reimport created content duplicates %d", n)
		}
		return err
	})
	// Prove actual effective app_runtime identity and RLS, independently of the
	// provider's explicit tenant predicates. Even a deliberately wrong predicate
	// cannot read or insert the other tenant's data.
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		var role string
		if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
			return err
		}
		if role != "app_runtime" {
			t.Fatalf("RLS fixture effective role %s", role)
		}
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM app.products WHERE tenant_id=$1::uuid`, b).Scan(&n)
		if n != 0 {
			t.Fatalf("RLS leaked wrong-tenant SELECT %d", n)
		}
		return err
	})
	rlsErr := runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.products(tenant_id,product_code) VALUES($1::uuid,'RLS-FORBIDDEN')`, b)
		return err
	})
	var pgErr *pgxadapter.PgError
	if !errors.As(rlsErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("RLS wrong-tenant INSERT should fail 42501, got %v", rlsErr)
	}
	// A late preflight database conflict must not create the preceding valid row.
	err := runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"A-VALID", "Valid", "A-SKU", "en"}, []string{"Z-CONFLICT", "Conflict", "S1", "en"})))
		if len(report.Errors) != 1 || report.Errors[0].Row != 3 {
			t.Fatalf("%+v", report)
		}
		return err
	})
	if !errors.Is(err, contract.ErrCSVInvalid) {
		t.Fatalf("late conflict %v", err)
	}
	assertAbsent := func(code string) {
		t.Helper()
		run(a, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			err := tx.QueryRow(ctx, `SELECT count(*) FROM app.products WHERE tenant_id=$1::uuid AND product_code_normalized=$2`, a, code).Scan(&n)
			if n != 0 {
				t.Fatalf("partial write %s count%d", code, n)
			}
			return err
		})
	}
	assertAbsent("A-VALID")
	// Seed a valid maximum revision so the final row encounters a real SQL
	// overflow after earlier writes. Runner must roll back the entire batch.
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"Z-OVERFLOW", "Old", "", "en"})))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title) SELECT tenant_id,id,'en',9223372036854775807,'Old' FROM app.products WHERE tenant_id=$1::uuid AND product_code_normalized='Z-OVERFLOW'`, a)
		return err
	})
	err = runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"A-ROLLBACK", "Valid", "A-ROLLBACK-SKU", "en"}, []string{"Z-OVERFLOW", "Changed", "", "en"})))
		if report.Imported != 0 {
			t.Fatalf("%+v", report)
		}
		return err
	})
	if err == nil {
		t.Fatal("SQL overflow swallowed")
	}
	assertAbsent("A-ROLLBACK")
	// A title upsert appends a revision, preserving body/SEO and listing state.
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,body_blocks,seo) SELECT tenant_id,id,'en',2,'Previous','[{"kind":"text"}]','{"keep":true}' FROM app.products WHERE tenant_id=$1::uuid AND product_code_normalized='P2'`, a)
		if err != nil {
			return err
		}
		_, err = port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"P2", "Updated", "", "en"})))
		if err != nil {
			return err
		}
		var title, body, seo string
		err = tx.QueryRow(ctx, `SELECT title,body_blocks::text,seo::text FROM app.product_content_revisions WHERE tenant_id=$1::uuid AND product_id=(SELECT id FROM app.products WHERE tenant_id=$1::uuid AND product_code_normalized='P2') AND revision=3`, a).Scan(&title, &body, &seo)
		if title != "Updated" || !strings.Contains(body, "text") || !strings.Contains(seo, "true") {
			t.Fatalf("lost content %s %s %s", title, body, seo)
		}
		return err
	})
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.products SET lifecycle_status='archived' WHERE tenant_id=$1::uuid AND product_code_normalized='P2'`, a)
		return err
	})
	err = runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"P2", "Archived change", "", "en"})))
		if len(report.Errors) != 1 || report.Errors[0].Code != "resource_archived" {
			t.Fatalf("%+v", report)
		}
		return err
	})
	if !errors.Is(err, contract.ErrCSVInvalid) {
		t.Fatal(err)
	}
	run(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.product_options(tenant_id,product_id,code) SELECT tenant_id,id,'color' FROM app.products WHERE tenant_id=$1::uuid AND product_code_normalized='P1'`, a)
		return err
	})
	err = runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		report, err := port.Import(ctx, tx, strings.NewReader(csvPGInput([]string{"P1", "Cannot flatten options", "S1", "en"})))
		if len(report.Errors) != 1 || report.Errors[0].Code != "unsupported_options" {
			t.Fatalf("%+v", report)
		}
		return err
	})
	if !errors.Is(err, contract.ErrCSVInvalid) {
		t.Fatal(err)
	}
	err = runner.InTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error { _, err := port.Export(ctx, tx); return err })
	if !errors.Is(err, contract.ErrCSVUnsupported) {
		t.Fatalf("options export %v", err)
	}
}
