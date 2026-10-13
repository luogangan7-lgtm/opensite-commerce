// Responsibility: guard preview's read-only transaction and owner-read path.
// Dependencies: checkout public ports; no SQL or provider fixture.
// Invariant: a missing cart cannot trigger checkout's write transaction.
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

type missingPreviewCart struct{}

func (missingPreviewCart) Read(context.Context, pgx.Tx, contract.AuthorizedActor, string) (contract.Cart, error) {
	return contract.Cart{}, contract.ErrNotFound
}

type unusedPreviewRules struct{}

func (unusedPreviewRules) Load(context.Context, pgx.Tx, contract.AuthorizedActor, contract.Cart, contract.ShippingAddress) (contract.Rules, error) {
	return contract.Rules{}, errors.New("unexpected rules load")
}

func TestPreviewUsesReadOnlyPortBeforeAnyWrite(t *testing.T) {
	actor, err := contract.NewAuthorizedActor("10000000-0000-4000-8000-000000000001", "guest", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	reads, writes := 0, 0
	s := New(contract.Config{Run: func(context.Context, string, func(context.Context, pgx.Tx) error) error { writes++; return nil }, Read: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		reads++
		return fn(ctx, nil)
	}, SnapshotCart: missingPreviewCart{}, Rules: unusedPreviewRules{}})
	_, err = s.Preview(context.Background(), actor, contract.PreviewInput{CartID: "20000000-0000-4000-8000-000000000001", Country: "US", PostalCode: "00001"})
	if !errors.Is(err, contract.ErrNotFound) || reads != 1 || writes != 0 {
		t.Fatalf("preview boundary: err=%v reads=%d writes=%d", err, reads, writes)
	}
}
