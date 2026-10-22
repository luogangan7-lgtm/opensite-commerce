//go:build pgx

// Responsibility: exercise public orders commands with generated SQL on real
// expendable PG18, including transaction rollback and tenant isolation.
// Dependencies: orders contract, pure quotes, pgx for an external test harness.
// Callers: tagged integration gates; all seeded data rolls back with the fixture.
package db_test

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	fulfillmentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
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
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
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
	f.errorAtSavepoint(t, func() error {
		_, e := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Fulfilled, EventKey: "shipment:event-1"})
		return e
	}, contract.ErrConflict)
	shipmentInput := fulfillmentcontract.CreateInput{OrderID: in.ID, ExpectedVersion: paid.Version, IdempotencyKey: "shipment:event-1", Carrier: "manual", TrackingNumber: "ORDER-GATE-1"}
	for _, line := range in.Lines {
		shipmentInput.Lines = append(shipmentInput.Lines, fulfillmentcontract.Line{OrderLineID: line.ID, Quantity: line.Quantity})
	}
	shipment, err := fulfillment.New().CreateShipment(f.ctx, f.tx, f.tenant, shipmentInput)
	if err != nil || shipment.Status != "shipped" || shipment.OrderVersion != 3 {
		t.Fatalf("shipment: %v %v", shipment, err)
	}
	fulfilled, err := svc.Transition(f.ctx, f.tx, f.tenant, contract.TransitionInput{OrderID: in.ID, Target: contract.Paid, EventKey: "verified-payment:event-1"})
	if err != nil || fulfilled.State != contract.Fulfilled || fulfilled.Version != 3 {
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
				// Trusted fixture switches to the same expiry capability as production jobs.
				if _, err := f.tx.Exec(f.ctx, "SET LOCAL ROLE app_queue_worker"); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := f.tx.QueryRow(f.ctx, "SELECT app.inventory_expire_due(100)").Scan(&count); err != nil || count != 1 {
					t.Fatalf("expire fixture: %d %v", count, err)
				}
				if _, err := f.tx.Exec(f.ctx, "SET LOCAL ROLE app_runtime"); err != nil {
					t.Fatal(err)
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

// Mixed explicit + automatic reservations for one SKU must not deadlock with a
// concurrent expiry sweep. Committed fixture data on separate connections: the
// create is parked (test-only trigger) right after its automatic reserve holds
// the stock row; the sweep then runs. Correct lock order (explicit reservation
// rows before any stock write) makes the sweep SKIP the held explicit row.
func TestOrdersServiceMixedReservationExpiryNoDeadlock(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connect := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
		return c
	}
	admin, gate, creator, sweeper := connect(), connect(), connect(), connect()
	f := orderFixture{ctx: ctx}
	var product, explicit string
	orderID := uuid.NewString()
	lockKey := int64(time.Now().UnixNano() & 0x7fffffff)
	trigger := "c15a_mixed_" + strings.ReplaceAll(orderID[:8], "-", "")
	for _, step := range []struct {
		sql  string
		args []any
		out  *string
	}{
		{"INSERT INTO app.tenants(name) VALUES('orders-mixed-race') RETURNING id::text", nil, &f.tenant},
		{"INSERT INTO app.products(tenant_id,product_code) VALUES($1,'MIXED-P') RETURNING id::text", []any{&f.tenant}, &product},
		{"INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'ORDER-SKU','default') RETURNING id::text", []any{&f.tenant, &product}, &f.sku},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*string))
		}
		if err := admin.QueryRow(ctx, step.sql, args...).Scan(step.out); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "SELECT set_config('app.tenant_id',$1,false)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "SELECT app.inventory_adjust($1,10,'mixed race receipt',$2,'inbound')", f.sku, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT (app.inventory_reserve($1,2,'order',$2,'mixed-explicit',900)).id::text", f.sku, orderID).Scan(&explicit); err != nil {
		t.Fatal(err)
	}
	// Due now: the sweep would take it unless the create already holds the row.
	if _, err := admin.Exec(ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND id=$2", f.tenant, explicit); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE FUNCTION public."+trigger+"() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN PERFORM pg_advisory_xact_lock_shared("+strconv.FormatInt(lockKey, 10)+"); RETURN NEW; END $b$"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP FUNCTION IF EXISTS public."+trigger+"() CASCADE")
	})
	if _, err := admin.Exec(ctx, "CREATE TRIGGER "+trigger+" BEFORE INSERT ON app.inventory_movements FOR EACH ROW WHEN (NEW.kind='reserve' AND NEW.owner_id='"+orderID+"'::uuid) EXECUTE FUNCTION public."+trigger+"()"); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	in := f.input(t)
	in.ID = orderID
	auto := f.input(t).Lines[0]
	in.Lines[0].ReservationID = explicit
	in.Lines = append(in.Lines, auto)
	begin := func(c *pgx.Conn) pgx.Tx {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	waiting := func(pid uint32, event string) bool {
		var n int
		_ = admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND ($2='' OR wait_event=$2)", pid, event).Scan(&n)
		return n == 1
	}
	createTx := begin(creator)
	createErr := make(chan error, 1)
	go func() { _, err := orders.New().Create(ctx, createTx, f.tenant, in); createErr <- err }()
	for i := 0; !waiting(creator.PgConn().PID(), "advisory"); i++ {
		if i > 300 {
			t.Fatal("create never parked after automatic reserve")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sweepTx := begin(sweeper)
	// Expendable fixture connection holds the scheduler role, never runtime expiry.
	if _, err := sweepTx.Exec(ctx, "SET LOCAL ROLE app_queue_worker"); err != nil {
		t.Fatal(err)
	}
	type swept struct {
		n   int
		err error
	}
	sweepDone := make(chan swept, 1)
	go func() {
		var n int
		err := sweepTx.QueryRow(ctx, "SELECT app.inventory_expire_due(100)").Scan(&n)
		sweepDone <- swept{n, err}
	}()
	var sweep swept
	sweepFinished := false
	for i := 0; i < 300 && !sweepFinished; i++ { // finished, or blocked on the create's stock lock
		select {
		case sweep = <-sweepDone:
			sweepFinished = true
		default:
			if waiting(sweeper.PgConn().PID(), "") {
				i = 300
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := gate.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	err := <-createErr
	_ = createTx.Rollback(ctx)
	if !sweepFinished {
		sweep = <-sweepDone
	}
	_ = sweepTx.Rollback(ctx)
	if errors.Is(err, contract.ErrRetry) || (sweep.err != nil && strings.Contains(sweep.err.Error(), "40P01")) {
		t.Fatalf("ORDERS_MIXED_LOCK_GATE deadlock: create=%v sweep=%v", err, sweep.err)
	}
	if !sweepFinished || sweep.err != nil || sweep.n != 0 {
		t.Fatalf("ORDERS_MIXED_LOCK_GATE sweep must skip the held explicit row without waiting: finished=%v n=%d err=%v", sweepFinished, sweep.n, sweep.err)
	}
	if !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("create with expired explicit hold: got %v want ErrConflict", err)
	}
}
