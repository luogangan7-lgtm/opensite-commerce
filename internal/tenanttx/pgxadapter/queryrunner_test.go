// Responsibilities: prove the public central runner rejects a URL tenant without
// trusted routing. Dependencies: sqlc callback signature and platform stamp;
// Go gates call this test; no PostgreSQL or fabricated authenticated context.
package pgxadapter_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"testing"
)

func TestRunAuthorizedRejectsUnstampedContext(t *testing.T) {
	var runner *pgxadapter.QueryRunner
	called := false
	for _, run := range []func(context.Context, string, func(*db.Queries) error) error{runner.RunAuthorized, runner.RunAuthorizedSnapshot} {
		err := run(context.Background(), "11111111-1111-1111-1111-111111111111", func(*db.Queries) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("unstamped request reached database callback")
		}
	}
	for _, run := range []func(context.Context, string, func(context.Context, pgx.Tx) error) error{runner.RunAuthorizedTx, runner.RunAuthorizedTxSnapshot} {
		err := run(context.Background(), "11111111-1111-1111-1111-111111111111", func(context.Context, pgx.Tx) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("unstamped command reached database callback")
		}
	}
}
