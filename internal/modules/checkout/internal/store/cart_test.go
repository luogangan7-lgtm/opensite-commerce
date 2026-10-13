// Responsibility: cart intent consumer admission and projection regressions.
// Dependencies: public cart/checkout contracts and a supplied tx marker only.
// Invariants: actor comes from trusted constructor, no alternate tx or prices.
package store

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	cart "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

const cartTenant = "10000000-0000-4000-8000-000000000001"
const cartID = "10000000-0000-4000-8000-000000000002"
const cartSKU = "10000000-0000-4000-8000-000000000003"
const cartCustomer = "10000000-0000-4000-8000-000000000004"

type cartTx struct{ pgx.Tx }

func TestCartAdapterUsesSuppliedActorTransactionAndIntent(t *testing.T) {
	for _, kind := range []string{"guest", "customer"} {
		t.Run(kind, func(t *testing.T) {
			id := cartCustomer
			if kind == "guest" {
				id = hex.EncodeToString(make([]byte, 32))
			}
			actor, err := contract.NewAuthorizedActor(cartTenant, kind, id)
			if err != nil {
				t.Fatal(err)
			}
			tx := &cartTx{}
			calls := 0
			reader := NewCart(func(_ context.Context, supplied pgx.Tx, a cart.AuthorizedActor, requested string) (cart.Intent, error) {
				calls++
				if supplied != tx || a.TenantID != cartTenant || requested != cartID {
					t.Fatal("supplied transaction or identity changed")
				}
				if kind == "guest" && (a.CustomerID != "" || hex.EncodeToString(a.GuestTokenHash) != id) {
					t.Fatal("guest was not the verified capability digest")
				}
				if kind == "customer" && (a.CustomerID != id || len(a.GuestTokenHash) != 0) {
					t.Fatal("customer actor changed")
				}
				return cart.Intent{ID: cartID, TenantID: cartTenant, Currency: "CAD", Version: 7, Lines: []cart.IntentLine{{SKUID: cartSKU, Quantity: 2}}}, nil
			})
			out, err := reader.Read(context.Background(), tx, actor, cartID)
			if err != nil || calls != 1 || out.ID != cartID || out.Currency != "CAD" || out.Version != 7 || len(out.Lines) != 1 || out.Lines[0].SKUID != cartSKU || out.Lines[0].Quantity != 2 || !tenanttx.ValidTenantID(out.Lines[0].ID) {
				t.Fatalf("intent %#v err %v calls%d", out, err, calls)
			}
			// Currency is preserved for the no-FX guard, while every price and
			// sellability rule is recomputed by SQLRules and calculators.
		})
	}
}
func TestCartAdapterRejectsForeignOrMalformedIntent(t *testing.T) {
	a, _ := contract.NewAuthorizedActor(cartTenant, "customer", cartCustomer)
	for _, tc := range []struct {
		name   string
		intent cart.Intent
	}{
		{"wrong tenant", cart.Intent{ID: cartID, TenantID: cartCustomer, Version: 1}},
		{"wrong cart", cart.Intent{ID: cartCustomer, TenantID: cartTenant, Version: 1}},
		{"zero version", cart.Intent{ID: cartID, TenantID: cartTenant}},
		{"bad SKU", cart.Intent{ID: cartID, TenantID: cartTenant, Version: 1, Lines: []cart.IntentLine{{SKUID: "bad", Quantity: 1}}}},
		{"negative quantity", cart.Intent{ID: cartID, TenantID: cartTenant, Version: 1, Lines: []cart.IntentLine{{SKUID: cartSKU, Quantity: -1}}}},
		{"duplicate SKU", cart.Intent{ID: cartID, TenantID: cartTenant, Version: 1, Lines: []cart.IntentLine{{SKUID: cartSKU, Quantity: 1}, {SKUID: cartSKU, Quantity: 1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := NewCart(func(context.Context, pgx.Tx, cart.AuthorizedActor, string) (cart.Intent, error) {
				return tc.intent, nil
			})
			if _, err := reader.Read(context.Background(), &cartTx{}, a, cartID); err == nil {
				t.Fatal("foreign or malformed cart intent admitted")
			}
		})
	}
}
func TestCartAdapterDefaultDenyAndStableErrors(t *testing.T) {
	a, _ := contract.NewAuthorizedActor(cartTenant, "customer", cartCustomer)
	if _, err := NewCart(nil).Read(context.Background(), &cartTx{}, a, cartID); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("nil reader: %v", err)
	}
	calls := 0
	read := NewCart(func(context.Context, pgx.Tx, cart.AuthorizedActor, string) (cart.Intent, error) {
		calls++
		return cart.Intent{}, apperrors.New(404, "cart_not_found", "Cart not found")
	})
	if _, err := read.Read(context.Background(), nil, a, cartID); err == nil {
		t.Fatal("nil tx admitted")
	}
	if _, err := read.Read(context.Background(), &cartTx{}, contract.AuthorizedActor{}, cartID); !errors.Is(err, contract.ErrForbidden) {
		t.Fatalf("zero actor: %v", err)
	}
	if calls != 0 {
		t.Fatal("invalid actor/tx accessed reader")
	}
	if _, err := read.Read(context.Background(), &cartTx{}, a, cartID); !errors.Is(err, contract.ErrNotFound) {
		t.Fatalf("stable miss: %v", err)
	}
}
