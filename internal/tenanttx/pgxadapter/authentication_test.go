// Responsibilities: prove the role-admitted query-only authentication port fails closed.
// Dependencies: adapter and sqlc query types; callers: CI default/race suites.
// Invariants: no callback before a usable dedicated identity runner, no raw transaction port.
package pgxadapter_test

import (
	"context"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type authenticationQueryRunner interface {
	RunAuthentication(context.Context, func(context.Context, *db.Queries) error) error
}

func authenticationPort(t *testing.T, runner *pgxadapter.IdentityRunner) authenticationQueryRunner {
	t.Helper()
	port, ok := any(runner).(authenticationQueryRunner)
	if !ok {
		t.Fatal("dedicated authentication requires a role-admitted query-only port")
	}
	return port
}

func TestAuthenticationPortFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *pgxadapter.IdentityRunner
	}{
		{name: "nil_runner"},
		{name: "unavailable_runner", runner: &pgxadapter.IdentityRunner{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := authenticationPort(t, tc.runner)
			called := false
			if err := port.RunAuthentication(context.Background(), func(context.Context, *db.Queries) error {
				called = true
				return nil
			}); err == nil || called {
				t.Fatal("unavailable authentication runner admitted a callback")
			}
			if err := port.RunAuthentication(context.Background(), nil); err == nil {
				t.Fatal("authentication runner admitted a nil callback")
			}
		})
	}
}
