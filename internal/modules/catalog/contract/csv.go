// Responsibilities: catalog CSV consumer port, bounds and row diagnostics.
// Dependencies: caller-owned tenant pgx transaction; callers: data-rights service.
// Invariants: provider never commits; a non-nil Import error must roll back.
package contract

import (
	"context"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
)

const CSVMaxBytes = 1 << 20
const CSVMaxRows = 1000

// CSV uses the exact header product_code,title,sku,locale. Codes/SKUs normalize
// to trimmed uppercase; locale normalizes lowercase. An empty SKU represents a
// product without a variant. Only option-free catalogs are supported. Export
// contains only active products with lowercase locale tags. Products without
// content export their code as draft title and en as locale. Export
// prefixes unsafe spreadsheet cells (and leading apostrophes) with one apostrophe;
// import reverses that encoding. Upserts retain existing content body/SEO and
// never change a published revision pointer or an archived resource.
type CSVRowError struct {
	Row     int    `json:"row"` // logical CSV record, including header at row 1
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type CSVReport struct {
	Rows     int           `json:"rows"`
	Imported int           `json:"imported"`
	Errors   []CSVRowError `json:"errors"`
}

var ErrCSVInvalid = errors.New("catalog CSV validation failed")
var ErrCSVLimit = errors.New("catalog CSV exceeds its byte or row bound")
var ErrCSVUnsupported = errors.New("catalog CSV cannot represent configured options or archived variants")

// Preflight performs no writes; Import reruns preflight in the same transaction
// before the first write. DB failures return an error, requiring caller rollback.
// The caller supplies an authorized tenant transaction; RLS remains authoritative.
type CSVPort interface {
	Preflight(context.Context, pgx.Tx, io.Reader) (CSVReport, error)
	Import(context.Context, pgx.Tx, io.Reader) (CSVReport, error)
	Export(context.Context, pgx.Tx) ([]byte, error)
}
