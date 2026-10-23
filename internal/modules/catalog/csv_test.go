// Responsibilities: CSV safety and whole-file validation gates.
// Dependencies: catalog CSV parser; callers: focused and full Go gates.
// Invariants: invalid batches never reach SQL; exports preserve CSV data safely.
package catalog

import (
	"encoding/csv"
	"strings"
	"testing"
)

func TestCSVParseRoundTrip(t *testing.T) {
	rows, report, err := parseCSV(strings.NewReader("product_code,title,sku,locale\n p1 ,\"Title, 中文\", s1 ,en\n"))
	if err != nil || len(report.Errors) != 0 || len(rows) != 1 || rows[0].ProductCode != "P1" || rows[0].SKU != "S1" || rows[0].Title != "Title, 中文" {
		t.Fatalf("%+v %+v %v", rows, report, err)
	}
}
func TestCSVWholeFileErrors(t *testing.T) {
	_, report, err := parseCSV(strings.NewReader("product_code,title,sku,locale\nP1,Good,S1,en\np1,Duplicate,S1,en\nP2,,S2,en\nP3,Good,S1,en\nP4,Good,S4,bad_locale\n"))
	if err != nil || len(report.Errors) != 4 || report.Errors[0].Row != 3 || report.Errors[3].Row != 6 {
		t.Fatalf("%+v %v", report, err)
	}
}
func TestCSVBoundsAndMalformed(t *testing.T) {
	for _, input := range []string{"wrong,title,sku,locale\n", "product_code,title,sku,locale\n\"unclosed", "product_code,title,sku,locale\n" + strings.Repeat("x", 1<<20), "product_code,title,sku,locale\n" + strings.Repeat("P,T,S,en\n", 1001)} {
		_, report, err := parseCSV(strings.NewReader(input))
		if err == nil && len(report.Errors) == 0 {
			t.Fatalf("accepted invalid input length=%d", len(input))
		}
	}
}
func TestCSVFormulaEscapeAndQuoting(t *testing.T) {
	values := []string{"=2+2", "+SUM(A1)", "-1", "@cmd", "\t=1", "\r=1", " \t@cmd", "'literal", "Text,\"quote\"\n中文", ""}
	for _, value := range values {
		safe := spreadsheetCell(value)
		if value != "" && (strings.ContainsAny(value[:1], "=+-@\t\r") || value == " \t@cmd" || value == "'literal") && safe != "'"+value {
			t.Fatalf("unsafe cell %q -> %q", value, safe)
		}
		var out strings.Builder
		w := csv.NewWriter(&out)
		_ = w.Write([]string{safe, "sentinel"})
		w.Flush()
		record, err := csv.NewReader(strings.NewReader(out.String())).Read()
		if err != nil || len(record) != 2 || unescapeSpreadsheetCell(record[0]) != value {
			t.Fatalf("roundtrip %q %q %v", value, out.String(), err)
		}
	}
}

func TestCSVRejectNonCanonicalCodes(t *testing.T) {
	for _, input := range []string{"product_code,title,sku,locale\n产品,T,S,en\n", "product_code,title,sku,locale\nP,T,=cmd,en\n"} {
		_, report, err := parseCSV(strings.NewReader(input))
		if err != nil || len(report.Errors) != 1 {
			t.Fatalf("%+v %v", report, err)
		}
	}
}

func TestCSVRejectUnicodeBeforeCanonicalization(t *testing.T) {
	for _, code := range []string{"ß", "ı", "ſ", "\u00a0P1\u00a0"} {
		for _, field := range []int{0, 2} {
			record := []string{"P1", "Title", "S1", "en"}
			record[field] = code
			_, report, err := parseCSV(strings.NewReader(strings.Join(csvHeader, ",") + "\n" + strings.Join(record, ",") + "\n"))
			if err != nil || len(report.Errors) != 1 {
				t.Fatalf("Unicode code %q field %d: %+v %v", code, field, report, err)
			}
		}
	}
}
