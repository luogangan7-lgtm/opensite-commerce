// Responsibility: adapt authenticated cart intent to checkout's consumer port.
// Dependencies: only public cart/checkout contracts; called from checkout Start.
// Invariants: supplied tx, actor identity only, ignores every projected price.
package store

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	cart "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type Cart struct{ read cart.IntentReader }

func NewCart(read cart.IntentReader) *Cart { return &Cart{read: read} }
func (c *Cart) Read(ctx context.Context, tx pgx.Tx, actor contract.AuthorizedActor, id string) (contract.Cart, error) {
	if !actor.Valid() {
		return contract.Cart{}, contract.ErrForbidden
	}
	if !tenanttx.ValidTenantID(id) {
		return contract.Cart{}, contract.ErrInvalid
	}
	if c == nil || c.read == nil || tx == nil {
		return contract.Cart{}, contract.ErrUnavailable
	}
	a := cart.AuthorizedActor{TenantID: actor.TenantID()}
	if actor.Kind() == "customer" {
		a.CustomerID = actor.ID()
	} else {
		a.GuestTokenHash, _ = hex.DecodeString(actor.ID())
	}
	intent, err := c.read(ctx, tx, a, id)
	if err != nil {
		var failure *apperrors.Error
		if errors.As(err, &failure) {
			switch failure.Status {
			case 404:
				return contract.Cart{}, contract.ErrNotFound
			case 400, 422:
				return contract.Cart{}, contract.ErrInvalid
			case 401, 403:
				return contract.Cart{}, contract.ErrForbidden
			case 409, 412:
				return contract.Cart{}, contract.ErrConflict
			}
		}
		return contract.Cart{}, contract.ErrUnavailable
	}
	if intent.TenantID != actor.TenantID() || intent.ID != id || intent.Version <= 0 || len(intent.Lines) == 0 || len(intent.Lines) > cart.MaxLines {
		return contract.Cart{}, contract.ErrInvalid
	}
	out := contract.Cart{ID: intent.ID, Currency: intent.Currency, Version: intent.Version}
	seen := make(map[string]bool, len(intent.Lines))
	for _, line := range intent.Lines {
		if !tenanttx.ValidTenantID(line.SKUID) || line.Quantity < 1 || line.Quantity > cart.MaxQuantity || seen[line.SKUID] {
			return contract.Cart{}, contract.ErrInvalid
		}
		seen[line.SKUID] = true
		out.Lines = append(out.Lines, contract.CartLine{ID: uuid.NewSHA1(uuid.MustParse(intent.ID), []byte(line.SKUID)).String(), SKUID: line.SKUID, Quantity: line.Quantity})
	}
	return out, nil
}
