// Store error-mapping tests: a runner that cannot open or scope a transaction
// yields the declared 503, authorization errors pass through, and an RLS
// denial on a cart write after an owner deactivation is the hiding 404.
package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func status(err error) int {
	var failure *apperrors.Error
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}

func TestStoreRunnerFailuresAreUnavailable(t *testing.T) {
	actor := contract.AuthorizedActor{TenantID: "10000000-0000-4000-8000-000000000001", GuestTokenHash: make([]byte, 32)}
	cart := "10000000-0000-4000-8000-000000000002"
	fail := func(err error) Run {
		return func(context.Context, string, func(*db.Queries) error) error { return err }
	}
	for name, tc := range map[string]struct {
		run  Run
		want int
	}{
		"nil runner":   {nil, 503},
		"begin failed": {fail(fmt.Errorf("begin tenant transaction: %w", errors.New("dial tcp: refused"))), 503},
		"unauthorized": {fail(apperrors.New(401, "unauthenticated", "no stamp")), 401},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(tc.run, tc.run)
			if _, err := s.Read(context.Background(), actor, cart); status(err) != tc.want {
				t.Fatalf("read: %v", err)
			}
			if _, err := s.Create(context.Background(), actor, "USD"); status(err) != tc.want {
				t.Fatalf("create: %v", err)
			}
			if _, err := s.Change(context.Background(), actor, cart, cart, 1, nil, nil); status(err) != tc.want {
				t.Fatalf("change: %v", err)
			}
		})
	}
}

func TestRLSDenialOnWriteIsHiddenMiss(t *testing.T) {
	if got := status(hidden(fmt.Errorf("insert: %w", &pgxadapter.PgError{Code: "42501"}))); got != 404 {
		t.Fatalf("42501 mapped to %d", got)
	}
	other := &pgxadapter.PgError{Code: "23514"}
	if hidden(other) != error(other) {
		t.Fatal("unrelated database error was rewritten")
	}
}
