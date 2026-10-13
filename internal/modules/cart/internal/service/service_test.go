// Cart service tests assert externally visible intent, pricing and admission.
// The fake implements the persistence port only; real SQL and RLS have separate
// PostgreSQL gates. Security and arithmetic failures must not mutate intent.
package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/internal/service"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

const tenant = "10000000-0000-4000-8000-000000000001"
const cartID = "10000000-0000-4000-8000-000000000002"
const sku = "10000000-0000-4000-8000-000000000003"

type repository struct {
	mu     sync.Mutex
	state  contract.Snapshot
	owner  contract.AuthorizedActor
	active bool
}

func fixture() (*service.Service, *repository, contract.AuthorizedActor) {
	a := contract.AuthorizedActor{TenantID: tenant, GuestTokenHash: make([]byte, 32)}
	r := &repository{owner: a, active: true, state: contract.Snapshot{ID: cartID, TenantID: tenant, Currency: "USD", Version: 1, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), PriceLists: []pricing.PriceList{{TenantID: tenant, ID: "20000000-0000-4000-8000-000000000001", Currency: "USD", Status: "active", Audience: "all", StartsAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Entries: []pricing.Entry{{SKUID: sku, MinQuantity: 1, UnitAmount: 100}, {SKUID: sku, MinQuantity: 3, UnitAmount: 75}}}}}}
	return service.New(r), r, a
}

func (r *repository) allowed(a contract.AuthorizedActor) error {
	if a.TenantID != r.owner.TenantID || a.CustomerID != r.owner.CustomerID || string(a.GuestTokenHash) != string(r.owner.GuestTokenHash) {
		return apperrors.New(404, "cart_not_found", "Cart not found")
	}
	return nil
}
func (r *repository) Create(_ context.Context, a contract.AuthorizedActor, currency string) (contract.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owner = a
	r.state.Currency = currency
	return r.state, nil
}
func (r *repository) Read(_ context.Context, a contract.AuthorizedActor, id string) (contract.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.allowed(a); err != nil {
		return contract.Snapshot{}, err
	}
	if id != cartID {
		return contract.Snapshot{}, apperrors.New(404, "cart_not_found", "Cart not found")
	}
	return r.state, nil
}
func (r *repository) Change(_ context.Context, a contract.AuthorizedActor, id, s string, q int64, check, validate func(contract.Snapshot) error) (contract.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.allowed(a); err != nil {
		return contract.Snapshot{}, err
	}
	if id != cartID {
		return contract.Snapshot{}, apperrors.New(404, "cart_not_found", "Cart not found")
	}
	if err := check(r.state); err != nil {
		return contract.Snapshot{}, err
	}
	if q > 0 && (s != sku || !r.active) {
		return contract.Snapshot{}, apperrors.New(422, "sku_not_active", "SKU is not active")
	}
	old := r.state
	r.state.Lines = nil
	if q > 0 {
		r.state.Lines = []contract.StoredLine{{SKUID: s, Quantity: q, Active: true, AvailableQuantity: 2}}
	}
	r.state.Version++
	if err := validate(r.state); err != nil {
		r.state = old
		return contract.Snapshot{}, err
	}
	return r.state, nil
}

func code(err error) string {
	var e *apperrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestCartWriteAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		q         int64
		tag, want string
	}{{"missing", 1, "", "precondition_required"}, {"mismatch", 1, "\"v99-unknown\"", "precondition_failed"}, {"negative", -1, "\"x\"", "invalid_quantity"}, {"too large", 1000001, "\"x\"", "invalid_quantity"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, a := fixture()
			_, err := s.PutLine(context.Background(), a, cartID, sku, tc.q, tc.tag)
			if code(err) != tc.want {
				t.Fatalf("got %v (%s), want %s", err, code(err), tc.want)
			}
			if r.state.Version != 1 || len(r.state.Lines) != 0 {
				t.Fatal("rejected request changed intent")
			}
		})
	}
}

func TestAbsoluteQuantityAndLivePricing(t *testing.T) {
	s, r, a := fixture()
	ctx := context.Background()
	v, err := s.Get(ctx, a, cartID)
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.PutLine(ctx, a, cartID, sku, 3, contract.ETag(v))
	if err != nil {
		t.Fatal(err)
	}
	if v.Subtotal != 225 || len(v.Lines) != 1 || v.Lines[0].Quantity != 3 || v.Lines[0].UnitAmount != 75 || v.Lines[0].Sellable || v.Lines[0].AvailableQuantity != 2 {
		t.Fatalf("wrong live tier/stock hint: %+v", v)
	}
	tag := contract.ETag(v)
	r.state.PriceLists[0].Entries[1].UnitAmount = 80
	v2, err := s.Get(ctx, a, cartID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != v.Version || v2.Subtotal != 240 || contract.ETag(v2) == tag {
		t.Fatal("price projection did not invalidate strong ETag")
	}
	_, err = s.PutLine(ctx, a, cartID, sku, 3, tag)
	if code(err) != "precondition_failed" {
		t.Fatalf("stale price tag admitted: %v", err)
	}
	v2, err = s.PutLine(ctx, a, cartID, sku, 3, contract.ETag(v2))
	if err != nil || v2.Lines[0].Quantity != 3 {
		t.Fatalf("absolute retry increments intent: %+v %v", v2, err)
	}
	v2, err = s.PutLine(ctx, a, cartID, sku, 0, contract.ETag(v2))
	if err != nil || len(v2.Lines) != 0 || v2.Subtotal != 0 {
		t.Fatalf("zero must remove line: %+v %v", v2, err)
	}
}

func TestInvalidPriceDoesNotTrapCartIntent(t *testing.T) {
	s, r, a := fixture()
	ctx := context.Background()
	empty, err := s.Get(ctx, a, cartID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.PutLine(ctx, a, cartID, sku, 1, contract.ETag(empty))
	if err != nil {
		t.Fatal(err)
	}
	r.state.PriceLists = nil
	current, err := s.Get(ctx, a, cartID)
	if err != nil {
		t.Fatalf("expired quote must remain readable for removal: %v", err)
	}
	if contract.ETag(current) == contract.ETag(before) {
		t.Fatal("quote availability must invalidate old ETag")
	}
	removed, err := s.DeleteLine(ctx, a, cartID, sku, contract.ETag(current))
	if err != nil {
		t.Fatalf("remove expired line: %v", err)
	}
	if len(removed.Lines) != 0 || removed.Subtotal != 0 {
		t.Fatalf("remaining intent=%+v", removed)
	}
}

func TestForeignActorDoesNotReadCart(t *testing.T) {
	s, _, a := fixture()
	a.GuestTokenHash = append([]byte(nil), a.GuestTokenHash...)
	a.GuestTokenHash[0] = 1
	_, err := s.Get(context.Background(), a, cartID)
	if code(err) != "cart_not_found" {
		t.Fatalf("foreign guest: %v", err)
	}
}
