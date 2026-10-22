// Responsibilities: bounded CSV parsing and spreadsheet-safe reversible encoding.
// Dependencies: encoding/csv and catalog contract; callers: CSV provider.
// Invariants: whole-file diagnostics precede SQL writes; at most 1MiB/1000 rows.
package catalog

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
)

type csvRow struct {
	ProductCode, Title, SKU, Locale string
	Number                          int
	productID                       string
	variantID                       string
}

var csvLocale = regexp.MustCompile(`^[a-z]{2,8}(-[a-z0-9]{1,8})*$`)
var csvHeader = []string{"product_code", "title", "sku", "locale"}

func csvProblem(report *contract.CSVReport, row int, field, code, message string) {
	report.Errors = append(report.Errors, contract.CSVRowError{Row: row, Field: field, Code: code, Message: message})
}
func parseCSV(input io.Reader) ([]csvRow, contract.CSVReport, error) {
	report := contract.CSVReport{Errors: []contract.CSVRowError{}}
	if input == nil {
		return nil, report, contract.ErrCSVInvalid
	}
	data, err := io.ReadAll(io.LimitReader(input, contract.CSVMaxBytes+1))
	if err != nil {
		return nil, report, err
	}
	if len(data) > contract.CSVMaxBytes {
		return nil, report, contract.ErrCSVLimit
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, report, contract.ErrCSVInvalid
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, report, contract.ErrCSVInvalid
	}
	if len(header) != len(csvHeader) {
		return nil, report, contract.ErrCSVInvalid
	}
	for i, h := range header {
		if h != csvHeader[i] {
			return nil, report, contract.ErrCSVInvalid
		}
	}
	rows := []csvRow{}
	seen := map[string]bool{}
	products := map[string]string{}
	skus := map[string]string{}
	for number := 2; ; number++ {
		record, readErr := r.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		report.Rows++
		if report.Rows > contract.CSVMaxRows {
			return nil, report, contract.ErrCSVLimit
		}
		if readErr != nil {
			csvProblem(&report, number, "", "malformed_csv", "Malformed CSV record")
			break
		}
		if len(record) != 4 {
			csvProblem(&report, number, "", "column_count", "Expected four columns")
			continue
		}
		for i := range record {
			record[i] = unescapeSpreadsheetCell(record[i])
		}
		row := csvRow{ProductCode: strings.ToUpper(strings.TrimSpace(record[0])), Title: record[1], SKU: strings.ToUpper(strings.TrimSpace(record[2])), Locale: strings.ToLower(strings.TrimSpace(record[3])), Number: number}
		if normalized, codeErr := normalizeCode(row.ProductCode); codeErr != nil {
			csvProblem(&report, number, "product_code", "invalid_code", "Product code must contain 1–128 ASCII letters, digits, dot, underscore or hyphen")
		} else {
			row.ProductCode = normalized
		}
		if strings.TrimSpace(row.Title) == "" || len(row.Title) > 4096 {
			csvProblem(&report, number, "title", "invalid_title", "Title must contain 1–4096 bytes")
		}
		if row.SKU != "" {
			if normalized, codeErr := normalizeCode(row.SKU); codeErr != nil {
				csvProblem(&report, number, "sku", "invalid_sku", "SKU must contain 1–128 ASCII letters, digits, dot, underscore or hyphen")
			} else {
				row.SKU = normalized
			}
		}
		if len(row.Locale) > 64 || !csvLocale.MatchString(row.Locale) {
			csvProblem(&report, number, "locale", "invalid_locale", "A language tag is required")
		}
		key := row.ProductCode + "\x00" + row.Locale
		if seen[key] {
			csvProblem(&report, number, "product_code", "duplicate_product_locale", "Only one row per product and locale is allowed")
		}
		seen[key] = true
		if prior, ok := products[row.ProductCode]; ok && prior != row.SKU {
			csvProblem(&report, number, "sku", "multiple_skus", "Option-free products permit one consistent SKU")
		}
		products[row.ProductCode] = row.SKU
		if row.SKU != "" {
			if prior, ok := skus[row.SKU]; ok && prior != row.ProductCode {
				csvProblem(&report, number, "sku", "duplicate_sku", "SKU belongs to another product in this file")
			}
			skus[row.SKU] = row.ProductCode
		}
		rows = append(rows, row)
	}
	if report.Rows == 0 {
		csvProblem(&report, 1, "", "empty_csv", "At least one data row is required")
	}
	return rows, report, nil
}
func spreadsheetUnsafe(s string) bool {
	if s == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(s)
	return strings.ContainsRune("=+-@'", first) || unicode.IsSpace(first) || unicode.IsControl(first)
}
func spreadsheetCell(s string) string {
	if spreadsheetUnsafe(s) {
		return "'" + s
	}
	return s
}
func unescapeSpreadsheetCell(s string) string {
	if strings.HasPrefix(s, "'") && spreadsheetUnsafe(s[1:]) {
		return s[1:]
	}
	return s
}
