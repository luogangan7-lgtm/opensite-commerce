// Responsibilities: option-free CSV preflight, transactional upsert and export.
// Dependencies: caller-owned authorized tenant pgx Tx, catalog schema003/018.
// Callers: bootstrap injects CSVPort into data-rights transport.
// Invariants: no commits; all preflight conflicts precede writes; RLS and explicit
// tenant predicates both apply; published pointers and archived rows never change.
package catalog

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
)

type csvStore struct{}

func NewCSV() contract.CSVPort { return csvStore{} }
func csvTenant(ctx context.Context, tx pgx.Tx) (string, error) {
	if tx == nil {
		return "", contract.ErrCSVInvalid
	}
	var tenant string
	err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.tenant_id',true),'')`).Scan(&tenant)
	if err != nil {
		return "", err
	}
	if tenant == "" {
		return "", contract.ErrCSVInvalid
	}
	return tenant, nil
}
func (csvStore) preflight(ctx context.Context, tx pgx.Tx, input io.Reader) ([]csvRow, contract.CSVReport, string, error) {
	rows, report, err := parseCSV(input)
	if err != nil || len(report.Errors) > 0 {
		return rows, report, "", err
	}
	tenant, err := csvTenant(ctx, tx)
	if err != nil {
		return nil, report, "", err
	}
	// Serialize concurrent imports on every product code and SKU before any row
	// lock or insert, in one global order: new products/SKUs created by another
	// batch are then visible as existing rows instead of failing 23505 or
	// deadlocking on uncommitted unique-index entries.
	// ponytail: CSV-only advisory locks; catalog API writers can still race a new code.
	keys := make([]string, 0, 2*len(rows))
	for _, row := range rows {
		keys = append(keys, "p:"+row.ProductCode)
		if row.SKU != "" {
			keys = append(keys, "s:"+row.SKU)
		}
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('catalog-csv:'||$1||':'||k,0)) FROM (SELECT DISTINCT k FROM unnest($2::text[]) k ORDER BY k) ordered`, tenant, keys); err != nil {
		return nil, report, tenant, err
	}
	// Stable lock order prevents opposite-order imports from deadlocking each other.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ProductCode == rows[j].ProductCode {
			return rows[i].Locale < rows[j].Locale
		}
		return rows[i].ProductCode < rows[j].ProductCode
	})
	for i := range rows {
		row := &rows[i]
		var status string
		err = tx.QueryRow(ctx, `SELECT id::text,lifecycle_status FROM app.products WHERE product_code_normalized=$1 AND tenant_id=$2::uuid FOR UPDATE`, row.ProductCode, tenant).Scan(&row.productID, &status)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, report, tenant, err
		}
		if err == nil {
			if status == "archived" {
				csvProblem(&report, row.Number, "product_code", "resource_archived", "Archived products cannot be imported")
			}
			var options bool
			var count int64
			var variantSKU string
			var unsupported bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app.product_options WHERE tenant_id=$2::uuid AND product_id=$1::uuid),count(*),coalesce(min(id::text),''),coalesce(min(sku_normalized),''),coalesce(bool_or(lifecycle_status='archived' OR option_key<>''),false) FROM (SELECT * FROM app.product_variants WHERE product_id=$1::uuid AND tenant_id=$2::uuid FOR UPDATE) locked_variants`, row.productID, tenant).Scan(&options, &count, &row.variantID, &variantSKU, &unsupported)
			if err != nil {
				return nil, report, tenant, err
			}
			if options || unsupported || count > 1 {
				csvProblem(&report, row.Number, "sku", "unsupported_options", "CSV supports one active option-free SKU only")
			} else if count == 1 && variantSKU != row.SKU {
				csvProblem(&report, row.Number, "sku", "sku_mismatch", "Existing variant SKU must match; CSV does not rename or remove variants")
			}
		}
		if row.SKU != "" {
			var owner string
			err = tx.QueryRow(ctx, `SELECT product_id::text FROM app.product_variants WHERE sku_normalized=$1 AND tenant_id=$2::uuid`, row.SKU, tenant).Scan(&owner)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, report, tenant, err
			}
			if err == nil && owner != row.productID {
				csvProblem(&report, row.Number, "sku", "sku_conflict", "SKU already belongs to another product")
			}
		}
	}
	sort.Slice(report.Errors, func(i, j int) bool { return report.Errors[i].Row < report.Errors[j].Row })
	return rows, report, tenant, nil
}
func (store csvStore) Preflight(ctx context.Context, tx pgx.Tx, input io.Reader) (contract.CSVReport, error) {
	_, report, _, err := store.preflight(ctx, tx, input)
	return report, err
}
func (store csvStore) Import(ctx context.Context, tx pgx.Tx, input io.Reader) (contract.CSVReport, error) {
	rows, report, tenant, err := store.preflight(ctx, tx, input)
	if err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		return report, contract.ErrCSVInvalid
	}
	ids := map[string]string{}
	variants := map[string]bool{}
	for _, row := range rows {
		product := row.productID
		if product == "" {
			product = ids[row.ProductCode]
		}
		if product == "" {
			err = tx.QueryRow(ctx, `INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES($1::uuid,$2,'draft') RETURNING id::text`, tenant, row.ProductCode).Scan(&product)
			if err != nil {
				return report, err
			}
			ids[row.ProductCode] = product
		}
		if row.SKU != "" && row.variantID == "" && !variants[product] {
			_, err = tx.Exec(ctx, `INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1::uuid,$2::uuid,$3,'')`, tenant, product, row.SKU)
			if err != nil {
				return report, err
			}
			variants[product] = true
		}
		// Content is append-only. Retain body and SEO from the latest locale revision,
		// skip identical titles, and leave any published listing pointer untouched.
		_, err = tx.Exec(ctx, `INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,body_blocks,seo)
SELECT $1::uuid,$2::uuid,$3,coalesce(latest.revision,0)+1,$4,coalesce(latest.body_blocks,'[]'::jsonb),coalesce(latest.seo,'{}'::jsonb)
FROM (SELECT 1) anchor LEFT JOIN LATERAL(SELECT revision,title,body_blocks,seo FROM app.product_content_revisions WHERE tenant_id=$1::uuid AND product_id=$2::uuid AND locale=$3 ORDER BY revision DESC LIMIT 1) latest ON true
WHERE latest.title IS DISTINCT FROM $4`, tenant, product, row.Locale, row.Title)
		if err != nil {
			return report, err
		}
	}
	report.Imported = report.Rows
	return report, nil
}
func (csvStore) Export(ctx context.Context, tx pgx.Tx) ([]byte, error) {
	tenant, err := csvTenant(ctx, tx)
	if err != nil {
		return nil, err
	}
	// A product with no copy exports a code-derived draft title/locale. Configured
	// options and archived variants cannot be represented, so fail explicitly.
	rows, err := tx.Query(ctx, `SELECT p.product_code,coalesce(c.title,p.product_code),coalesce(v.sku,''),coalesce(c.locale,'en'),
EXISTS(SELECT 1 FROM app.product_options o WHERE o.tenant_id=p.tenant_id AND o.product_id=p.id) OR coalesce(v.option_key<>'',false) OR coalesce(v.lifecycle_status='archived',false)
FROM app.products p LEFT JOIN app.product_variants v ON v.tenant_id=p.tenant_id AND v.product_id=p.id
LEFT JOIN LATERAL(SELECT DISTINCT ON(locale) locale,title FROM app.product_content_revisions WHERE tenant_id=p.tenant_id AND product_id=p.id ORDER BY locale,revision DESC) c ON true
WHERE p.tenant_id=$1::uuid AND p.lifecycle_status<>'archived' ORDER BY p.product_code_normalized,c.locale,v.sku_normalized LIMIT 1001`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	if err = writer.Write(csvHeader); err != nil {
		return nil, err
	}
	count := 0
	for rows.Next() {
		count++
		if count > contract.CSVMaxRows {
			return nil, contract.ErrCSVLimit
		}
		var code, title, sku, locale string
		var unsupported bool
		if err = rows.Scan(&code, &title, &sku, &locale, &unsupported); err != nil {
			return nil, err
		}
		if unsupported {
			return nil, contract.ErrCSVUnsupported
		}
		// Match import field limits so every successful export is importable.
		if len(strings.TrimSpace(code)) > 128 || len(sku) > 128 || len(title) > 4096 || len(locale) > 64 {
			return nil, contract.ErrCSVLimit
		}
		if _, codeErr := normalizeCode(code); codeErr != nil {
			return nil, contract.ErrCSVInvalid
		}
		if sku != "" {
			if _, skuErr := normalizeCode(sku); skuErr != nil {
				return nil, contract.ErrCSVInvalid
			}
		}
		if locale != strings.ToLower(locale) || !csvLocale.MatchString(locale) || strings.TrimSpace(title) == "" {
			return nil, contract.ErrCSVInvalid
		}
		if err = writer.Write([]string{spreadsheetCell(code), spreadsheetCell(title), spreadsheetCell(sku), spreadsheetCell(locale)}); err != nil {
			return nil, err
		}
		writer.Flush()
		if err = writer.Error(); err != nil {
			return nil, err
		}
		if output.Len() > contract.CSVMaxBytes {
			return nil, contract.ErrCSVLimit
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
