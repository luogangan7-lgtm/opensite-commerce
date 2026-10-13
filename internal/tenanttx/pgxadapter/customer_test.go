// Responsibility: verify customer capability fallback and argument rejection.
// Dependencies: public adapter API; GitHub CI executes both build modes.
// Invariants: invalid tenant/canceled/nil runners never reach query callbacks.
package pgxadapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestCustomerRunnerRejectsBeforeQueries(t *testing.T) {
	var runner *pgxadapter.CustomerRunner
	runner.Close()
	called := false
	fn := func(*db.Queries) error { called = true; return nil }
	if err := runner.InTenant(context.Background(), "not-a-tenant", fn); !errors.Is(err, tenanttx.ErrInvalidTenant) || called {
		t.Fatal("invalid tenant reached queries")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runner.InTenant(ctx, "11111111-1111-4111-8111-111111111111", fn); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled request reached queries")
	}
	if err := runner.InTenant(context.Background(), "11111111-1111-4111-8111-111111111111", fn); err == nil || called {
		t.Fatal("nil capability reached queries")
	}
}
