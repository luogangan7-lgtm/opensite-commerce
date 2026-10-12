//go:build pgx

// Responsibility: exercise public orders commands with generated SQL on real
// expendable PG18, including transaction rollback and tenant isolation.
// Dependencies: orders contract, pure quotes, pgx for an external test harness.
// Callers: tagged integration gates; all seeded data rolls back with the fixture.
package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

type orderFixture struct {
	ctx                  context.Context
	tx                   pgx.Tx
	tenant, foreign, sku string
}

func newOrderFixture(t *testing.T) orderFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	f := orderFixture{ctx: ctx, tx: tx}
	var product string
	if err = tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('orders-go-gate') RETURNING id::text").Scan(&f.tenant); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('orders-go-foreign') RETURNING id::text").Scan(&f.foreign); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, "INSERT INTO app.products(tenant_id,product_code) VALUES($1,'ORDER-P') RETURNING id::text", f.tenant).Scan(&product); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, "INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'ORDER-SKU','default') RETURNING id::text", f.tenant, product).Scan(&f.sku); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT app.inventory_adjust($1,30,'order test receipt',$2,'inbound')", f.sku, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f orderFixture) input(t *testing.T) contract.CreateInput {
	t.Helper()
	line := uuid.NewString()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	zone := uuid.NewString()
	rate := uuid.NewString()
	price, err := pricing.Quote([]pricing.PriceList{{TenantID: f.tenant, ID: uuid.NewString(), Currency: "USD", Status: "active", Audience: "all", StartsAt: at.Add(-time.Hour), Entries: []pricing.Entry{{SKUID: f.sku, MinQuantity: 1, UnitAmount: 100}}}}, pricing.Request{TenantID: f.tenant, SKUID: f.sku, Currency: "USD", Quantity: 2, At: at})
	if err != nil {
		t.Fatal(err)
	}
	taxes, err := tax.Calculate(tax.Destination{Country: "US"}, []tax.LineItem{{ID: line, Currency: "USD", Category: "standard", UnitAmount: price.Unit.Amount, Quantity: 2}}, false, at, tax.Ruleset{Zones: []tax.Zone{{ID: zone, Locations: []tax.Destination{{Country: "US"}}}}, Categories: []tax.Category{{Code: "standard"}}, Rates: []tax.Rate{{ID: rate, ZoneID: zone, Category: "standard", BasisPoints: 1000, ValidFrom: at.Add(-time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	shippingQuote, err := shipping.Quote([]shipping.Zone{{ID: zone, Destinations: []shipping.Destination{{Country: "US"}}}}, []shipping.Rate{{ID: rate, ProfileID: zone, ZoneID: zone, Method: "fixed", Currency: "USD", Amount: 50}}, shipping.Request{ProfileID: zone, Destination: shipping.Destination{Country: "US"}, Subtotal: money.Money{Amount: 200, Currency: "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	return contract.CreateInput{ID: uuid.NewString(), IdempotencyKey: "create:" + uuid.NewString(), Currency: "USD", Buyer: contract.Buyer{Guest: contract.Contact{Email: "guest@example.test"}}, Lines: []contract.Line{{ID: line, SKUID: f.sku, SKU: "ORDER-SKU", Quantity: 2, Price: price, Tax: taxes.Lines[0]}}, Shipping: shippingQuote}
}
func (f orderFixture) errorAtSavepoint(t *testing.T, fn func() error, want error) {
	t.Helper()
	if _, e := f.tx.Exec(f.ctx, "SAVEPOINT expected_error"); e != nil {
		t.Fatal(e)
	}
	err := fn()
	if !errors.Is(err, want) {
		t.Fatalf("got %v; want %v", err, want)
	}
	if _, e := f.tx.Exec(f.ctx, "ROLLBACK TO SAVEPOINT expected_error"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.tx.Exec(f.ctx, "RELEASE SAVEPOINT expected_error"); e != nil {
		t.Fatal(e)
	}
}
func TestOrdersServiceProductionQueries(t *testing.T) {
	f := newOrderFixture(t)
	svc := orders.New()
	in := f.input(t)
	created, err := svc.Create(f.ctx, f.tx, f.tenant, in)
	if err != nil {
		t.Fatal(err)
	}
	if created.Total != (money.Money{Amount: 270, Currency: "USD"}) || created.State != contract.PendingPayment || created.Version != 1 || created.Number < 1 {
		t.Fatalf("created: %+v", created)
	}
	payable, err := svc.GetPayable(f.ctx, f.tx, f.tenant, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.GetPayable(f.ctx, f.tx, f.tenant, in.ID)
	if err != nil || payable != again || payable.Amount != created.Total || payable.IntentKey == "" || payable.Version != 1 {
		t.Fatalf("payable %v / %v: %v", payable, again, err)
	}
	replay, err := svc.Create(f.ctx, f.tx, f.tenant, in)
	if err != nil || replay != created {
		t.Fatalf("creation replay: %v %v", replay, err)
	}
	f.errorAtSavepoint(t, func() error { _, e := svc.GetPayable(f.ctx, f.tx, f.foreign, in.ID); return e }, contract.ErrNotFound)
	f.errorAtSavepoint(t, func() error { _, e := svc.GetPayable(f.ctx, f.tx, f.tenant, uuid.NewString()); return e }, contract.ErrNotFound)

	f.errorAtSavepoint(t, func() error {
		if _, e := f.tx.Exec(f.ctx, "SELECT set_config('app.tenant_id','',true)"); e != nil {
			return e
		}
		_, e := svc.GetPayable(f.ctx, f.tx, f.tenant, in.ID)
		return e
	}, contract.ErrNotFound)

	wrongContextCreate := f.input(t)
	f.errorAtSavepoint(t, func() error { _, e := svc.Create(f.ctx, f.tx, f.foreign, wrongContextCreate); return e }, contract.ErrNotFound)
	changed := in
	changed.Shipping.Charge.Amount++
	f.errorAtSavepoint(t, func() error { _, e := svc.Create(f.ctx, f.tx, f.tenant, changed); return e }, contract.ErrKeyReused)
	paid, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Paid, EventKey: "verified-payment:event-1"})
	if err != nil || paid.Version != 2 {
		t.Fatalf("pay: %v %v", paid, err)
	}
	replay, err = svc.Create(f.ctx, f.tx, f.tenant, in)
	if err != nil || replay != paid {
		t.Fatalf("creation replay after commit: %v %v", replay, err)
	}
	f.errorAtSavepoint(t, func() error { _, e := svc.GetPayable(f.ctx, f.tx, f.tenant, in.ID); return e }, contract.ErrConflict)

	f.errorAtSavepoint(t, func() error {
		_, e := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Cancelled, EventKey: "cancel-after-paid"})
		return e
	}, contract.ErrConflict)
	fulfilled, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Fulfilled, EventKey: "shipment:event-1"})
	if err != nil || fulfilled.Version != 3 {
		t.Fatalf("fulfill: %v %v", fulfilled, err)
	}
	oldReplay, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Paid, EventKey: "verified-payment:event-1"})
	if err != nil || oldReplay != fulfilled {
		t.Fatalf("old event replay after advancement: %v %v", oldReplay, err)
	}

	f.errorAtSavepoint(t, func() error {
		_, e := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Cancelled, EventKey: "verified-payment:event-1"})
		return e
	}, contract.ErrKeyReused)
	var reserved, onHand int64
	var movements, events int
	if err = f.tx.QueryRow(f.ctx, "SELECT reserved,on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&reserved, &onHand); err != nil {
		t.Fatal(err)
	}
	if err = f.tx.QueryRow(f.ctx, "SELECT count(*) FROM app.inventory_movements WHERE tenant_id=$1 AND kind='commit'", f.tenant).Scan(&movements); err != nil {
		t.Fatal(err)
	}
	if err = f.tx.QueryRow(f.ctx, "SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND delivery_target='orders.events'", f.tenant).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || onHand != 28 || movements != 1 || events != 3 {
		t.Fatalf("side-effect conservation reserved=%d on_hand=%d commits=%d events=%d", reserved, onHand, movements, events)
	}
	partial, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.PartiallyRefunded, EventKey: "refund:part-1", RefundMinor: 50})
	if err != nil || partial.State != contract.PartiallyRefunded {
		t.Fatalf("partial refund: %v %v", partial, err)
	}
	full, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Refunded, EventKey: "refund:part-2", RefundMinor: 220})
	if err != nil || full.State != contract.Refunded {
		t.Fatalf("full refund: %v %v", full, err)
	}
	f.errorAtSavepoint(t, func() error {
		_, e := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Paid, EventKey: "paid-illegal"})
		return e
	}, contract.ErrConflict)
}
func TestOrdersServiceAtomicRollback(t *testing.T) {
	f := newOrderFixture(t)
	svc := orders.New()
	in := f.input(t)
	child, err := f.tx.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Create(f.ctx, child, f.tenant, in); err != nil {
		t.Fatal(err)
	}
	if err = child.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	var ordersCount, reservations, events int
	var reserved, onHand int64
	if err = f.tx.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM app.orders WHERE tenant_id=$1),(SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1),(SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1)", f.tenant).Scan(&ordersCount, &reservations, &events); err != nil {
		t.Fatal(err)
	}
	if err = f.tx.QueryRow(f.ctx, "SELECT reserved,on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&reserved, &onHand); err != nil {
		t.Fatal(err)
	}
	if ordersCount != 0 || reservations != 0 || events != 0 || reserved != 0 || onHand != 30 {
		t.Fatalf("rollback leaked: orders=%d reservations=%d events=%d reserved=%d onhand=%d", ordersCount, reservations, events, reserved, onHand)
	}
}
func TestOrdersServiceReservationOwnership(t *testing.T) {
	f := newOrderFixture(t)
	svc := orders.New()
	original := f.input(t)
	if _, err := svc.Create(f.ctx, f.tx, f.tenant, original); err != nil {
		t.Fatal(err)
	}
	var reservation string
	if err := f.tx.QueryRow(f.ctx, "SELECT reservation_id::text FROM app.order_lines WHERE tenant_id=$1 AND order_id=$2", f.tenant, original.ID).Scan(&reservation); err != nil {
		t.Fatal(err)
	}
	forged := f.input(t)
	forged.Lines[0].ReservationID = reservation
	f.errorAtSavepoint(t, func() error { _, e := svc.Create(f.ctx, f.tx, f.tenant, forged); return e }, contract.ErrReservationOwner)
	cancelled, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: original.ID, Target: contract.Cancelled, EventKey: "cancel:1"})
	if err != nil || cancelled.State != contract.Cancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	f.errorAtSavepoint(t, func() error {
		_, e := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: original.ID, Target: contract.Paid, EventKey: "paid-after-cancel"})
		return e
	}, contract.ErrConflict)
}

func TestOrdersServiceExpiredReservationCancel(t *testing.T) {
	for _, expiredByWorker := range []bool{false, true} {
		name := "active deadline passed"
		if expiredByWorker {
			name = "worker already expired hold"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrderFixture(t)
			svc := orders.New()
			in := f.input(t)
			if _, err := svc.Create(f.ctx, f.tx, f.tenant, in); err != nil {
				t.Fatal(err)
			}
			// The fixture administrator advances only its own reservation's deadline.
			if _, err := f.tx.Exec(f.ctx, "RESET ROLE"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tx.Exec(f.ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND owner_id=$2", f.tenant, in.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tx.Exec(f.ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				t.Fatal(err)
			}
			if expiredByWorker {
				var count int
				if err := f.tx.QueryRow(f.ctx, "SELECT app.inventory_expire_due(100)").Scan(&count); err != nil || count != 1 {
					t.Fatalf("expire fixture: %d %v", count, err)
				}
			}
			cancelled, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Cancelled, EventKey: "expired-cancel:1"})
			if err != nil || cancelled.State != contract.Cancelled {
				t.Fatalf("cancel expired hold: %+v %v", cancelled, err)
			}
			var reserved, onHand int64
			if err = f.tx.QueryRow(f.ctx, "SELECT reserved,on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&reserved, &onHand); err != nil {
				t.Fatal(err)
			}
			if reserved != 0 || onHand != 30 {
				t.Fatalf("expired cancel stock: %d/%d", reserved, onHand)
			}
		})
	}
}

func TestOrdersServiceCustomerBuyer(t *testing.T) {
	f := newOrderFixture(t)
	svc := orders.New()
	customer, foreignCustomer := uuid.NewString(), uuid.NewString()
	// Fixture setup is privileged; the service still runs as the tenant runtime.
	// A customer row alone is not authentication evidence. The caller is trusted
	// to bind authenticated buyer identity before invoking the order command.
	if _, err := f.tx.Exec(f.ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tx.Exec(f.ctx, "INSERT INTO app.customers(tenant_id,id,email,display_name) VALUES($1,$2,'order-customer@example.test','Order buyer'),($3,$4,'foreign-customer@example.test','Foreign buyer')", f.tenant, customer, f.foreign, foreignCustomer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tx.Exec(f.ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatal(err)
	}
	in := f.input(t)
	in.Buyer = contract.Buyer{CustomerID: customer}
	created, err := svc.Create(f.ctx, f.tx, f.tenant, in)
	if err != nil || created.State != contract.PendingPayment || created.Total.Amount != 270 {
		t.Fatalf("registered customer order: %+v %v", created, err)
	}
	var storedCustomer string
	var guestNull bool
	if err = f.tx.QueryRow(f.ctx, "SELECT customer_id::text,guest_contact IS NULL FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, in.ID).Scan(&storedCustomer, &guestNull); err != nil {
		t.Fatal(err)
	}
	if storedCustomer != customer || !guestNull {
		t.Fatalf("customer snapshot mismatch: %s guest_null=%v", storedCustomer, guestNull)
	}
	replay, err := svc.Create(f.ctx, f.tx, f.tenant, in)
	if err != nil || replay != created {
		t.Fatalf("registered creation replay: %+v %v", replay, err)
	}
	for _, invalidCustomer := range []string{foreignCustomer, uuid.NewString()} {
		refused := f.input(t)
		refused.Buyer = contract.Buyer{CustomerID: invalidCustomer}
		f.errorAtSavepoint(t, func() error { _, e := svc.Create(f.ctx, f.tx, f.tenant, refused); return e }, contract.ErrInvalid)
	}
	var ordersCount, reservations, events int
	var reserved, onHand int64
	if err = f.tx.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM app.orders WHERE tenant_id=$1),(SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1),(SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND delivery_target='orders.events')", f.tenant).Scan(&ordersCount, &reservations, &events); err != nil {
		t.Fatal(err)
	}
	if err = f.tx.QueryRow(f.ctx, "SELECT reserved,on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&reserved, &onHand); err != nil {
		t.Fatal(err)
	}
	if ordersCount != 1 || reservations != 1 || events != 1 || reserved != 2 || onHand != 30 {
		t.Fatalf("customer rejection leaked writes: orders=%d reservations=%d events=%d reserved=%d on_hand=%d", ordersCount, reservations, events, reserved, onHand)
	}
}
