//go:build pgx

// Responsibility: real Cart-to-Checkout transaction and ownership witnesses.
// Dependencies: public Cart intent reader, Checkout/Orders/Ledger and pgxadapter.
// Invariants: guest/customer RLS, exact GUC restoration, frozen intent/version,
// no currency conversion or rejected-request effects; no raw connection/pool.
package db_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cartcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
)

func realCheckoutCart(t *testing.T, f *checkoutPGFixture, customer bool, currency string) {
	t.Helper()
	var customerID any
	var guest []byte
	if customer {
		customerID = uuid.NewString()
		var err error
		f.actor, err = checkoutcontract.NewAuthorizedActor(f.tenant, "customer", customerID.(string))
		if err != nil {
			t.Fatal(err)
		}
	} else {
		guest, _ = hex.DecodeString(f.actor.ID())
	}
	err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if customer {
			if _, err := tx.Exec(ctx, "INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,'real-cart@example.invalid')", f.tenant, customerID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO app.carts(tenant_id,id,customer_id,guest_token_hash,currency,version) VALUES($1,$2,$3,$4,$5,9)", f.tenant, f.cart.cart.ID, customerID, guest, currency); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity) VALUES($1,$2,$3,$4)", f.tenant, f.cart.cart.ID, f.sku, f.cart.cart.Lines[0].Quantity)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}})
	f.service = checkout.New(checkout.Config{Run: f.run, Cart: checkout.NewSQLCart(cart.ReadIntent), Rules: rules, Orders: orders.New(), Ledger: ledger.New(), Payment: f.provider})
}

func TestCheckoutPGRealCartGuestAndCustomer(t *testing.T) {
	for _, customer := range []bool{false, true} {
		t.Run(map[bool]string{false: "guest", true: "customer"}[customer], func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			realCheckoutCart(t, f, customer, "USD")
			// The caller already has settings. Cart must restore their exact values,
			// not erase them or leave the borrowed actor visible to another module.
			err := f.run(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				q := db.New(tx)
				prior := db.SetCartContextParams{TenantContext: f.tenant, CustomerContext: uuid.NewString(), TokenContext: hex.EncodeToString(make([]byte, 32))}
				if err := q.SetCartContext(ctx, prior); err != nil {
					return err
				}
				a := cartcontract.AuthorizedActor{TenantID: f.tenant}
				if customer {
					a.CustomerID = f.actor.ID()
				} else {
					a.GuestTokenHash, _ = hex.DecodeString(f.actor.ID())
				}
				intent, err := cart.ReadIntent(ctx, tx, a, f.cart.cart.ID)
				if err != nil {
					return err
				}
				if intent.Version != 9 || intent.Currency != "USD" || len(intent.Lines) != 1 || intent.Lines[0].SKUID != f.sku || intent.Lines[0].Quantity != 2 {
					t.Fatalf("real frozen intent %#v", intent)
				}
				after, err := q.GetCartContext(ctx)
				if err != nil {
					return err
				}
				if after.TenantContext != prior.TenantContext || after.CustomerContext != prior.CustomerContext || after.TokenContext != prior.TokenContext || after.RuntimeRole != "app_runtime" {
					t.Fatalf("borrowed actor leaked: %#v", after)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			in := f.input()
			if customer {
				in.BuyerContact = orderscontract.Contact{}
			}
			ctx := checkoutRequestContext(f.ctx)
			start, err := f.service.Start(ctx, f.actor, in)
			if err != nil || start.AmountMinor != 270 || start.Currency != "USD" {
				t.Fatalf("real cart start %#v %v", start, err)
			}
			paid, err := f.service.Confirm(ctx, f.event(start, payment.StatusAuthorized))
			if err != nil || paid.State != "paid" {
				t.Fatalf("real cart paid %#v %v", paid, err)
			}
			w := f.witness(t, start.OrderID)
			if w.state != "paid" || w.onHand != 8 || w.reserved != 0 || w.journals != 1 || w.debits != 540 || w.credits != 540 || w.cash != 270 || w.receivable != 0 || w.paidEvents != 1 {
				t.Fatalf("real cart paid witness %+v", w)
			}
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var version int64
				if err := tx.QueryRow(ctx, "SELECT cart_version FROM app.checkout_sessions WHERE tenant_id=$1 AND id=$2", f.tenant, start.CheckoutID).Scan(&version); err != nil {
					return err
				}
				if version != 9 {
					t.Fatalf("persisted cart version %d", version)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// A new key is a new purchase of the same shopping intent. Immutable
			// order line IDs must be namespaced per purchase, not reused cart IDs.
			secondInput := in
			secondInput.IdempotencyKey = "start:" + uuid.NewString()
			second, err := f.service.Start(ctx, f.actor, secondInput)
			if err != nil || second.CheckoutID == start.CheckoutID || second.OrderID == start.OrderID {
				t.Fatalf("second cart purchase %#v %v", second, err)
			}
			if replay, err := f.service.Start(ctx, f.actor, secondInput); err != nil || replay != second {
				t.Fatalf("second purchase replay %#v %v", replay, err)
			}
			if secondPaid, err := f.service.Confirm(ctx, f.event(second, payment.StatusAuthorized)); err != nil || secondPaid.State != "paid" {
				t.Fatalf("second purchase paid %#v %v", secondPaid, err)
			}
			for _, order := range []string{start.OrderID, second.OrderID} {
				w := f.witness(t, order)
				if w.onHand != 6 || w.reserved != 0 || w.journals != 1 || w.debits != 540 || w.credits != 540 || w.cash != 540 || w.receivable != 0 || w.paidEvents != 1 {
					t.Fatalf("two purchases witness %+v", w)
				}
			}
			if f.provider.captures.Load() != 2 {
				t.Fatalf("two purchases captured %d times", f.provider.captures.Load())
			}
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var rows, orders, lines int64
				if err := tx.QueryRow(ctx, "SELECT count(*),count(DISTINCT order_id),count(DISTINCT id) FROM app.order_lines WHERE tenant_id=$1 AND order_id IN($2,$3)", f.tenant, start.OrderID, second.OrderID).Scan(&rows, &orders, &lines); err != nil {
					return err
				}
				if rows != 2 || orders != 2 || lines != 2 {
					t.Fatalf("immutable purchase identities rows%d orders%d lines%d", rows, orders, lines)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckoutPGRealCartRejectsWrongActorAndCurrency(t *testing.T) {
	for _, kind := range []string{"wrong guest", "wrong customer", "foreign tenant", "currency mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 10, 2)
			currency := "USD"
			if kind == "currency mismatch" {
				currency = "CAD"
			}
			realCheckoutCart(t, f, kind == "wrong customer", currency)
			actor := f.actor
			want := checkoutcontract.ErrNotFound
			switch kind {
			case "wrong guest":
				actor, _ = checkoutcontract.NewAuthorizedActor(f.tenant, "guest", hex.EncodeToString(make([]byte, 32)))
			case "wrong customer":
				actor, _ = checkoutcontract.NewAuthorizedActor(f.tenant, "customer", uuid.NewString())
			case "foreign tenant":
				actor, _ = checkoutcontract.NewAuthorizedActor(uuid.NewString(), "guest", f.actor.ID())
			case "currency mismatch":
				want = checkoutcontract.ErrInvalid
			}
			in := f.input()
			if actor.Kind() == "customer" {
				in.BuyerContact = orderscontract.Contact{}
			}
			var creates int
			f.provider.before = func(_ context.Context, _ paymentfake.Operation) error { creates++; return nil }
			if _, err := f.service.Start(checkoutRequestContext(f.ctx), actor, in); !errors.Is(err, want) {
				t.Fatalf("%s was not denied: %v", kind, err)
			}
			if creates != 0 {
				t.Fatal("rejected cart accessed provider")
			}
			if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var effects int64
				if err := tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=$1)+(SELECT count(*) FROM app.orders WHERE tenant_id=$1)+(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1)+(SELECT reserved FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2)", f.tenant, f.sku).Scan(&effects); err != nil {
					return err
				}
				if effects != 0 {
					t.Fatalf("rejected cart had %d database effects", effects)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
