// Responsibility: compose opt-in cart checkout behind independently verified actors.
// Dependencies: Cart/Checkout/Orders/Ledger public ports and authorized pgxadapter runners.
// Caller: FromEnvironment; invariant: retain Payments receiver, staff rechecks and DB-clock rules.
package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cartcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
)

func wireStorefrontCheckout(c *Config, r *pgxadapter.QueryRunner) error {
	encoded := os.Getenv("OPENSITE_STOREFRONT_KEY")
	if encoded == "" {
		return nil
	}
	if c.Checkout == nil || (c.Checkout.Events == nil && c.Stripe == nil) || c.Checkout.Service != nil || c.Checkout.Resolver != nil {
		return errors.New("storefront checkout requires isolated Payments composition")
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) < 32 || len(key) > 128 {
		return errors.New("invalid storefront key")
	}
	tenant, origin := os.Getenv("OPENSITE_STOREFRONT_TENANT_ID"), os.Getenv("OPENSITE_STOREFRONT_ORIGIN")
	raw := os.Getenv("OPENSITE_CHECKOUT_RULE_METADATA")
	var metadata checkout.RuleMetadata
	if len(raw) > 65536 || json.Unmarshal([]byte(raw), &metadata) != nil || metadata.Profiles[tenant] == "" || len(metadata.SKUs[tenant]) == 0 {
		return errors.New("checkout tenant rule metadata missing")
	}
	resolver, err := checkouthttp.NewGuestResolver(checkouthttp.GuestConfig{TenantID: tenant, Origin: origin, Key: key, VerifyCart: func(ctx context.Context, a checkoutcontract.AuthorizedActor, id string) error {
		return r.RunCapabilitySnapshot(ctx, a.TenantID(), withCheckoutRuntimeRole(func(ctx context.Context, tx pgx.Tx) error {
			hash, err := hex.DecodeString(a.ID())
			if err != nil {
				return err
			}
			_, err = cart.ReadIntentSnapshot(ctx, tx, cartcontract.AuthorizedActor{TenantID: a.TenantID(), GuestTokenHash: hash}, id)
			return err
		}))
	}})
	if err != nil {
		return err
	}
	provider := c.Payment
	if provider == nil {
		provider = paymentfake.New()
	}
	c.Payment = provider
	c.Checkout.Service = checkout.New(checkout.Config{Run: func(ctx context.Context, t string, fn func(context.Context, pgx.Tx) error) error {
		return r.RunAuthorizedTx(ctx, t, withCheckoutRuntimeRole(fn))
	}, Read: func(ctx context.Context, t string, fn func(context.Context, pgx.Tx) error) error {
		return r.RunAuthorizedTxSnapshot(ctx, t, withCheckoutRuntimeRole(fn))
	}, Cart: checkout.NewSQLCart(cart.ReadIntent), SnapshotCart: checkout.NewSQLCart(cart.ReadIntentSnapshot), Rules: checkout.NewSQLRules(metadata), Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: provider})
	c.Checkout.Resolver, c.Checkout.Origin = resolver, origin
	return nil
}
func withCheckoutRuntimeRole(fn func(context.Context, pgx.Tx) error) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		if err := db.New(tx).SetCartRuntimeRole(ctx); err != nil {
			return err
		}
		return fn(ctx, tx)
	}
}
