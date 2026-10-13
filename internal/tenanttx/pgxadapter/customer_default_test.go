//go:build !pgx

// Responsibility: guard customer DB configuration in dependency-light builds.
// Dependencies: public adapter API; no database or real DSN is used by this test.
// Invariants: fallback never silently accepts explicit database configuration.
package pgxadapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestCustomerDatabaseRequiresPGX(t *testing.T) {
	runner, err := pgxadapter.OpenCustomerRunner(context.Background(), "invalid-private-configuration")
	if runner != nil || err == nil || strings.Contains(err.Error(), "private-configuration") {
		t.Fatal("fallback accepted or disclosed database configuration")
	}
}
