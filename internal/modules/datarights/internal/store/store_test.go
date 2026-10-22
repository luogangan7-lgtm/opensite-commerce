// Responsibilities: preserve stable domain errors without leaking PostgreSQL
// diagnostics. Caller transactions roll back busy privacy operations before retry.
package store

import (
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
)

func TestPrivacyContentionRequiresRetry(t *testing.T) {
	for _, code := range []string{"55P03", "40P01", "40001"} {
		t.Run(code, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &pgxadapter.PgError{Code: code, Message: "private diagnostic"})
			if got := MapError(err); !errors.Is(got, contract.ErrBusy) {
				t.Fatalf("contention mapped to %v, want retryable busy", got)
			}
		})
	}
	if got := MapError(&pgxadapter.PgError{Code: "XX000"}); !errors.Is(got, contract.ErrUnavailable) {
		t.Fatalf("unrecognized failure became retryable: %v", got)
	}
}
