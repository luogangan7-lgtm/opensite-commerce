// Responsibilities: pin import outcome mapping (threads T5/T10) without a database.
// Dependencies: catalog CSV contract errors; Go gates call it.
package catalogcsvhttp

import (
	"errors"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

func TestImportResponseStructuralErrorIs422(t *testing.T) {
	rows := contract.CSVReport{Rows: 1, Errors: []contract.CSVRowError{{Row: 2, Field: "locale", Code: "required", Message: "Locale is required"}}}
	if got, err := importResponse(rows, contract.ErrCSVInvalid); err != nil || got.Status != 200 {
		t.Fatalf("row diagnostics status=%d err=%v", got.Status, err)
	}
	if got, err := importResponse(contract.CSVReport{Rows: 1}, nil); err != nil || got.Status != 200 {
		t.Fatalf("success status=%d err=%v", got.Status, err)
	}
	_, err := importResponse(contract.CSVReport{}, contract.ErrCSVInvalid)
	var app *apperrors.Error
	if !errors.As(err, &app) || app.Status != 422 {
		t.Fatalf("structural failure not 422: %v", err)
	}
}
