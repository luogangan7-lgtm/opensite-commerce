//go:build !pgx

// Responsibilities: guard explicit DB configuration in the dependency-light
// runtime. Dependencies: public adapter Open; default Go tests call this;
// credentials must never be echoed in a startup failure.
package pgxadapter_test

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"strings"
	"testing"
)

func TestConfiguredDatabaseRequiresPGX(t *testing.T) {
	runner, err := pgxadapter.Open(context.Background(), "postgres://private-user:PRIVATE-SECRET@db.invalid/example")
	if err == nil || runner != nil {
		t.Fatal("configured DB silently accepted in default build")
	}
	if strings.Contains(err.Error(), "PRIVATE-SECRET") || strings.Contains(err.Error(), "private-user") {
		t.Fatal("credential leaked")
	}
}
