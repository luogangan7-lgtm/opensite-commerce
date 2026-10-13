// Responsibilities: verify CSV preflight-before-write and failure propagation.
// Dependencies: caller-owned pgx Tx seam; callers: focused and full Go gates.
// Invariants: late row conflicts cause zero writes, SQL failures cannot succeed.
package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
)

type csvTestRow func(...any) error

func (r csvTestRow) Scan(values ...any) error { return r(values...) }

type csvTestTx struct {
	pgx.Tx
	writes int
	fail   error
	row    func(string, []any) pgx.Row
}

func (tx *csvTestTx) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	return tx.row(q, args)
}
func (tx *csvTestTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	tx.writes++
	if tx.fail != nil {
		return pgconn.CommandTag{}, tx.fail
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func csvNewRows(q string, args []any) pgx.Row {
	return csvTestRow(func(dest ...any) error {
		switch {
		case strings.Contains(q, "current_setting"):
			*dest[0].(*string) = "00000000-0000-0000-0000-000000000001"
		case strings.Contains(q, "INSERT INTO app.products"):
			*dest[0].(*string) = "00000000-0000-0000-0000-000000000002"
		default:
			return pgx.ErrNoRows
		}
		return nil
	})
}
func TestCSVImportInvalidBatchNoSQL(t *testing.T) {
	tx := &csvTestTx{row: func(string, []any) pgx.Row { t.Fatal("invalid whole file reached SQL"); return nil }}
	report, err := NewCSV().Import(context.Background(), tx, strings.NewReader("product_code,title,sku,locale\nP1,Good,S1,en\nP2,,S2,en\n"))
	if !errors.Is(err, contract.ErrCSVInvalid) || tx.writes != 0 || len(report.Errors) != 1 {
		t.Fatalf("%+v %v writes%d", report, err, tx.writes)
	}
}
func TestCSVImportLateDatabaseConflictNoWrites(t *testing.T) {
	tx := &csvTestTx{row: func(q string, args []any) pgx.Row {
		if strings.Contains(q, "sku_normalized") && args[0] == "S2" {
			return csvTestRow(func(dest ...any) error { *dest[0].(*string) = "foreign-product"; return nil })
		}
		return csvNewRows(q, args)
	}}
	report, err := NewCSV().Import(context.Background(), tx, strings.NewReader("product_code,title,sku,locale\nP1,Good,S1,en\nP2,Good,S2,en\n"))
	if !errors.Is(err, contract.ErrCSVInvalid) || tx.writes != 0 || len(report.Errors) != 1 || report.Errors[0].Row != 3 {
		t.Fatalf("%+v %v writes%d", report, err, tx.writes)
	}
}
func TestCSVImportSQLFailureReturned(t *testing.T) {
	failed := errors.New("injected SQL failure")
	tx := &csvTestTx{row: csvNewRows, fail: failed}
	report, err := NewCSV().Import(context.Background(), tx, strings.NewReader("product_code,title,sku,locale\nP1,Good,,en\n"))
	if !errors.Is(err, failed) || report.Imported != 0 {
		t.Fatalf("swallowed transaction failure: %+v %v", report, err)
	}
}

type csvTestRows struct {
	pgx.Rows
	items   [][]any
	offset  int
	failure error
}

func (r *csvTestRows) Close()     {}
func (r *csvTestRows) Err() error { return r.failure }
func (r *csvTestRows) Next() bool { r.offset++; return r.offset <= len(r.items) }
func (r *csvTestRows) Scan(dest ...any) error {
	for i, value := range r.items[r.offset-1] {
		switch d := dest[i].(type) {
		case *string:
			*d = value.(string)
		case *bool:
			*d = value.(bool)
		}
	}
	return nil
}

type csvExportTx struct {
	csvTestTx
	rows *csvTestRows
}

func (tx *csvExportTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return tx.rows, nil }
func TestCSVExportRoundTrip(t *testing.T) {
	tx := &csvExportTx{csvTestTx: csvTestTx{row: csvNewRows}, rows: &csvTestRows{items: [][]any{{"-CODE", "=SUM(1,2)\n中文", "-SKU", "en", false}, {"P2", "'literal", "", "en", false}}}}
	output, err := NewCSV().Export(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	rows, report, err := parseCSV(strings.NewReader(string(output)))
	if err != nil || len(report.Errors) > 0 || len(rows) != 2 || rows[0].ProductCode != "-CODE" || rows[0].Title != "=SUM(1,2)\n中文" || rows[1].Title != "'literal" {
		t.Fatalf("%q %+v %+v %v", output, rows, report, err)
	}
	if !strings.Contains(string(output), "'-CODE") || !strings.Contains(string(output), "'=SUM") {
		t.Fatalf("unsafe spreadsheet output %q", output)
	}
}
func TestCSVExportUnsupportedAndReadFailure(t *testing.T) {
	failure := errors.New("read failed")
	for _, test := range []struct {
		rows *csvTestRows
		want error
	}{{&csvTestRows{items: [][]any{{"P", "T", "S", "en", true}}}, contract.ErrCSVUnsupported}, {&csvTestRows{failure: failure}, failure}} {
		tx := &csvExportTx{csvTestTx: csvTestTx{row: csvNewRows}, rows: test.rows}
		output, err := NewCSV().Export(context.Background(), tx)
		if !errors.Is(err, test.want) || output != nil {
			t.Fatalf("output=%q err=%v", output, err)
		}
	}
}

func TestCSVPreflightExistingArchiveAndOptions(t *testing.T) {
	for _, test := range []struct {
		status  string
		options bool
		want    string
	}{{"archived", false, "resource_archived"}, {"draft", true, "unsupported_options"}} {
		tx := &csvTestTx{row: func(q string, args []any) pgx.Row {
			switch {
			case strings.Contains(q, "SELECT id::text,lifecycle_status"):
				return csvTestRow(func(dest ...any) error { *dest[0].(*string) = "product"; *dest[1].(*string) = test.status; return nil })
			case strings.Contains(q, "count(*)"):
				return csvTestRow(func(dest ...any) error {
					*dest[0].(*bool) = test.options
					*dest[1].(*int64) = 1
					*dest[2].(*string) = "variant"
					*dest[3].(*string) = "S1"
					*dest[4].(*bool) = false
					return nil
				})
			case strings.Contains(q, "sku_normalized"):
				return csvTestRow(func(dest ...any) error { *dest[0].(*string) = "product"; return nil })
			default:
				return csvNewRows(q, args)
			}
		}}
		report, err := NewCSV().Preflight(context.Background(), tx, strings.NewReader("product_code,title,sku,locale\nP1,Good,S1,en\n"))
		if err != nil || tx.writes != 0 || len(report.Errors) != 1 || report.Errors[0].Code != test.want {
			t.Fatalf("%+v %v writes%d", report, err, tx.writes)
		}
	}
}

func TestCSVExportBounds(t *testing.T) {
	for _, test := range []struct {
		count int
		title string
	}{{1001, "T"}, {300, strings.Repeat("T", 4096)}} {
		values := make([][]any, test.count)
		for i := range values {
			values[i] = []any{"P", test.title, "S", "en", false}
		}
		tx := &csvExportTx{csvTestTx: csvTestTx{row: csvNewRows}, rows: &csvTestRows{items: values}}
		out, err := NewCSV().Export(context.Background(), tx)
		if out != nil || !errors.Is(err, contract.ErrCSVLimit) {
			t.Fatalf("unbounded export count%d bytes%d err%v", test.count, len(out), err)
		}
	}
}
