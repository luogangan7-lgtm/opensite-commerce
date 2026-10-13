// Responsibilities: guard the transaction entry used by HTTP idempotency and
// River producers. Dependencies: central tenant admission; callback must never
// run from an unstamped URL tenant, in either default or pgx builds.
package pgxadapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestRunAuthorizedTxRejectsUnstampedContext(t *testing.T) {
	var runner *pgxadapter.QueryRunner
	called := false
	err := runner.RunAuthorizedTx(context.Background(), "11111111-1111-1111-1111-111111111111", func(context.Context, pgx.Tx) error {
		called = true
		return nil
	})
	var admission *apperrors.Error
	if err == nil || called || !errors.As(err, &admission) || admission.Code != "unauthenticated" {
		t.Fatal("unstamped request reached transaction callback")
	}
}
