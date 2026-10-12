// Line-limit test: a cart at contract.MaxLines rejects a new SKU before commit
// but still accepts quantity changes and removals of existing lines.
package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/internal/service"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

// fullRepo applies the absolute mutation to a copy and commits only when
// check and validate both pass, like the PostgreSQL store's transaction.
type fullRepo struct{ state contract.Snapshot }

func (r *fullRepo) Create(context.Context, contract.AuthorizedActor, string) (contract.Snapshot, error) {
	return r.state, nil
}
func (r *fullRepo) Read(context.Context, contract.AuthorizedActor, string) (contract.Snapshot, error) {
	return r.state, nil
}
func (r *fullRepo) Change(_ context.Context, _ contract.AuthorizedActor, _, s string, q int64, check, validate func(contract.Snapshot) error) (contract.Snapshot, error) {
	if err := check(r.state); err != nil {
		return contract.Snapshot{}, err
	}
	after := r.state
	after.Lines = nil
	found := false
	for _, line := range r.state.Lines {
		if line.SKUID == s {
			found = true
			if q == 0 {
				continue
			}
			line.Quantity = q
		}
		after.Lines = append(after.Lines, line)
	}
	if !found && q > 0 {
		after.Lines = append(after.Lines, contract.StoredLine{SKUID: s, Quantity: q, Active: true})
	}
	after.Version++
	if err := validate(after); err != nil {
		return contract.Snapshot{}, err
	}
	r.state = after
	return after, nil
}

func skuID(i int) string { return fmt.Sprintf("30000000-0000-4000-8000-%012d", i) }

func TestCartLineLimitRejectsNewSKUBeforeCommit(t *testing.T) {
	list := pricing.PriceList{TenantID: tenant, ID: "20000000-0000-4000-8000-000000000001", Currency: "USD", Status: "active", Audience: "all", StartsAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	state := contract.Snapshot{ID: cartID, TenantID: tenant, Currency: "USD", Version: 1, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for i := 0; i <= contract.MaxLines; i++ {
		list.Entries = append(list.Entries, pricing.Entry{SKUID: skuID(i), MinQuantity: 1, UnitAmount: 1})
		if i < contract.MaxLines {
			state.Lines = append(state.Lines, contract.StoredLine{SKUID: skuID(i), Quantity: 1, Active: true})
		}
	}
	state.PriceLists = []pricing.PriceList{list}
	repo := &fullRepo{state: state}
	svc := service.New(repo)
	actor := contract.AuthorizedActor{TenantID: tenant, GuestTokenHash: make([]byte, 32)}
	etag := func() string {
		view, err := service.Project(repo.state)
		if err != nil {
			t.Fatal(err)
		}
		return contract.ETag(view)
	}
	_, err := svc.PutLine(context.Background(), actor, cartID, skuID(contract.MaxLines), 1, etag())
	var failure *apperrors.Error
	if !errors.As(err, &failure) || failure.Status != 422 || failure.Code != "cart_line_limit" {
		t.Fatalf("new SKU beyond limit: %v", err)
	}
	if repo.state.Version != 1 || len(repo.state.Lines) != contract.MaxLines {
		t.Fatal("rejected over-limit line persisted")
	}
	if _, err = svc.PutLine(context.Background(), actor, cartID, skuID(0), 2, etag()); err != nil {
		t.Fatalf("existing line update at limit: %v", err)
	}
	if _, err = svc.DeleteLine(context.Background(), actor, cartID, skuID(1), etag()); err != nil {
		t.Fatalf("removal at limit: %v", err)
	}
	if _, err = svc.PutLine(context.Background(), actor, cartID, skuID(contract.MaxLines), 1, etag()); err != nil {
		t.Fatalf("new SKU after removal: %v", err)
	}
}
