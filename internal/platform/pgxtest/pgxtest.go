// Package pgxtest owns the one "fixture missing" policy for PostgreSQL tests.
// It depends only on os and testing; pgx-tagged *_test.go files call Skip.
// Invariant: OPENSITE_REQUIRE_PGX_FIXTURES=1 (set by CI) turns a skip into a failure.
package pgxtest

import (
	"os"
	"testing"
)

// Skip skips locally when a DB fixture env is absent, but fails under CI.
// Subprocess-only helpers launched by a parent test must keep plain t.Skip.
func Skip(t testing.TB, msg string) {
	t.Helper()
	if os.Getenv("OPENSITE_REQUIRE_PGX_FIXTURES") == "1" {
		t.Fatal(msg + " (OPENSITE_REQUIRE_PGX_FIXTURES=1 forbids skipping)")
	}
	t.Skip(msg)
}
