//go:build pgx

// Responsibilities: exercise fulfillment/returns public ports on real PG18.
// Dependencies: tenanttx adapter, public modules, money and test Payment Fake.
// Callers: tagged integration gate. Invariants: no raw pool/connection, tenant
// hiding, committed paid stock, durable PSP recovery and balanced refund ledger.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	fulfillmentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	ledgercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type fulfillmentRunner struct {
	runner pgxadapter.Runner
	role   string
}

func (r fulfillmentRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	return r.runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		// These two literal roles come from test wiring, never client input.
		statement := "SET LOCAL ROLE app_runtime"
		if r.role == "service" {
			statement = "SET LOCAL ROLE app_returns_service"
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

type fulfillmentCapture struct {
	captured returnscontract.CapturedPayment
}

func (r fulfillmentCapture) CapturedPayment(_ context.Context, _ pgx.Tx, tenant, order string) (returnscontract.CapturedPayment, error) {
	if r.captured.TenantID != tenant || r.captured.OrderID != order {
		return returnscontract.CapturedPayment{}, returnscontract.ErrNotFound
	}
	return r.captured, nil
}

type fulfillmentAccounts struct {
	tenant   string
	accounts returnscontract.RefundAccounts
}

func (a fulfillmentAccounts) RefundAccounts(_ context.Context, _ pgx.Tx, tenant, currency string) (returnscontract.RefundAccounts, error) {
	if tenant != a.tenant || currency != "USD" {
		return returnscontract.RefundAccounts{}, returnscontract.ErrNotFound
	}
	return a.accounts, nil
}

type failingRefundJournal struct {
	payments.RefundJournal
	fail   bool
	ledger *ledger.Module
}

func (l *failingRefundJournal) PostReturnRefund(ctx context.Context, tx pgx.Tx, id, key, refund string, in ledgercontract.Transaction) (string, error) {
	if l.fail {
		l.fail = false
		return "", errors.New("injected post-provider journal failure")
	}
	return l.RefundJournal.PostReturnRefund(ctx, tx, id, key, refund, in)
}

func (l *failingRefundJournal) Post(ctx context.Context, tx pgx.Tx, in ledgercontract.Transaction) (string, error) {
	if l.fail {
		l.fail = false
		return "", errors.New("injected post-provider journal failure")
	}
	return l.ledger.Post(ctx, tx, in)
}

type fulfillmentFixture struct {
	ctx                        context.Context
	maint                      pgxadapter.Runner
	runtime, service           fulfillmentRunner
	tenant, foreign, sku, line string
	order                      orderscontract.Order
	payments                   *paymentfake.Fake
	returns                    returnscontract.Service
	journal                    *failingRefundJournal
}

func newFulfillmentFixture(t *testing.T) fulfillmentFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable migrated PG18 DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	r, closePool, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePool)
	f := fulfillmentFixture{ctx: ctx, maint: r, runtime: fulfillmentRunner{runner: r}, service: fulfillmentRunner{runner: r, role: "service"}, tenant: uuid.NewString(), foreign: uuid.NewString(), sku: uuid.NewString(), line: uuid.NewString(), payments: paymentfake.New()}
	accounts := returnscontract.RefundAccounts{Income: uuid.NewString(), Tax: uuid.NewString(), Shipping: uuid.NewString(), Cash: uuid.NewString()}
	err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO app.tenants(id,name,settlement_currency) VALUES($1,'fulfillment gate','USD'),($2,'foreign gate','USD')", f.tenant, f.foreign); err != nil {
			return err
		}
		product := uuid.NewString()
		if _, err := tx.Exec(ctx, "INSERT INTO app.products(tenant_id,id,product_code) VALUES($1,$2,'FULFILLMENT-P')", f.tenant, product); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key) VALUES($1,$2,$3,'FULFILLMENT-SKU','default')", f.tenant, f.sku, product); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT app.inventory_adjust($1,20,'fulfillment fixture',$2,'inbound')", f.sku, uuid.NewString()); err != nil {
			return err
		}
		for _, a := range []struct{ id, code, kind string }{{accounts.Income, "sales", "revenue"}, {accounts.Tax, "tax", "liability"}, {accounts.Shipping, "shipping", "revenue"}, {accounts.Cash, "cash", "asset"}} {
			if _, err := tx.Exec(ctx, "INSERT INTO app.ledger_accounts(tenant_id,id,currency,code,account_type) VALUES($1,$2,'USD',$3,$4)", f.tenant, a.id, a.code, a.kind); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	zone, rate := uuid.NewString(), uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := orderscontract.CreateInput{ID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Buyer: orderscontract.Buyer{Guest: orderscontract.Contact{Email: "fixture@example.test"}}, Currency: "USD", Shipping: shipping.Result{Charge: money.Money{Amount: 40, Currency: "USD"}, ZoneID: zone, RateID: rate, Method: "fixed"}, Adjustments: []orderscontract.Adjustment{{ID: uuid.NewString(), Kind: "discount", AmountMinor: -20, Reason: "fixed gate discount"}}, Lines: []orderscontract.Line{{ID: f.line, SKUID: f.sku, SKU: "FULFILLMENT-SKU", Quantity: 4, Price: pricing.Result{Unit: money.Money{Amount: 100, Currency: "USD"}, Line: money.Money{Amount: 400, Currency: "USD"}, PriceListID: uuid.NewString(), MinQuantity: 1}, Tax: tax.LineResult{ID: f.line, Amounts: tax.Amounts{Net: 400, Tax: 40, Gross: 440}, Snapshot: tax.Snapshot{Currency: "USD", ZoneID: zone, RateID: rate, Category: "standard", BasisPoints: 1000, ValidFrom: &at, Rounding: "half_up", Reason: "rate"}}}}}
	err = f.runtime.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.order, err = orders.New().Create(ctx, tx, f.tenant, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	intentRequest := payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: f.order.ID, OrderVersion: f.order.Version, Amount: f.order.Total, IdempotencyKey: uuid.NewString()}
	intent, err := f.payments.CreateIntent(ctx, intentRequest)
	if err != nil {
		t.Fatal(err)
	}
	intentRequest.IdempotencyKey = "capture:" + uuid.NewString()
	if _, err = f.payments.Capture(ctx, payment.PaymentOperation{CreatePaymentIntent: intentRequest, ProviderRef: intent.ProviderRef}); err != nil {
		t.Fatal(err)
	}
	err = f.runtime.InTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.order, err = orders.New().Transition(ctx, tx, f.tenant, orderscontract.TransitionInput{OrderID: f.order.ID, Target: orderscontract.Paid, EventKey: "paid:" + uuid.NewString()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.journal = &failingRefundJournal{ledger: ledger.New()}
	f.returns = returns.New(returns.Config{Runner: f.service, Payments: f.payments, Captures: fulfillmentCapture{captured: returnscontract.CapturedPayment{TenantID: f.tenant, OrderID: f.order.ID, ProviderRef: intent.ProviderRef, OrderVersion: intentRequest.OrderVersion, Paid: intentRequest.Amount}}, Accounts: fulfillmentAccounts{tenant: f.tenant, accounts: accounts}, Ledger: f.journal})
	return f
}
func (f fulfillmentFixture) stock(t *testing.T) int64 {
	t.Helper()
	var stock int64
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&stock)
	})
	if err != nil {
		t.Fatal(err)
	}
	return stock
}
func TestFulfillmentRefundReconcileAfterCustomerErasure(t *testing.T) {
	f := newFulfillmentFixture(t)
	customer := uuid.NewString()
	err := f.maint.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,'erase-refund@example.invalid')", f.tenant, customer); err != nil {
			return err
		}
		// Seed the explicit mailbox-proven identity path, never email equality.
		_, err := tx.Exec(ctx, "INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id) VALUES($1,$2,$3)", f.tenant, customer, f.order.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var requested returnscontract.Return
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		requested, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: "0500dffb-2fbd-5351-98af-6c3f55008cdf", Kind: returnscontract.UnshippedCancel, Reason: "private customer return note", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 1}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.journal.fail = true
	refund := returnscontract.RefundInput{ReturnID: requested.ID, ExpectedVersion: requested.OrderVersion, IdempotencyKey: "95c03c4e-90b5-5799-8a82-ac0758075d87"}
	if _, err = f.returns.Refund(f.ctx, f.tenant, refund); err == nil {
		t.Fatal("post-provider failure was not injected")
	}
	err = f.service.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT app.returns_fail_refund($1,$2,false)", requested.ID, refund.IdempotencyKey)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT app.data_rights_erase_customer($1,'service',$2)", customer, uuid.NewString())
		return err
	})
	if err != nil {
		t.Fatalf("refund_reconcile prevented erasure: %v", err)
	}
	// Recovery must use the frozen provider/key/accounts after a restart, even
	// with no mutable capture/account resolver available.
	f.returns = returns.New(returns.Config{Runner: f.service, Payments: f.payments, Ledger: f.journal})
	completed, err := f.returns.Refund(f.ctx, f.tenant, refund)
	if err != nil || completed.Status != "refunded" || len(f.payments.Events()) != 3 {
		t.Fatalf("erasure broke durable provider recovery: %+v err=%v effects=%d", completed, err, len(f.payments.Events()))
	}
	if _, err = f.returns.Refund(f.ctx, f.tenant, refund); err != nil || len(f.payments.Events()) != 3 {
		t.Fatalf("erased refund replay repeated provider effect: err=%v effects=%d", err, len(f.payments.Events()))
	}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		late, err := f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: completed.OrderVersion, IdempotencyKey: "e52303fc-0a92-582e-87f1-8512dc97de68", Kind: returnscontract.UnshippedCancel, Reason: "late private customer note", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 1}}})
		if err == nil && !returnscontract.ValidID(late.ID) {
			t.Fatal("scrubbed receipt broke the public command result identity")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var clean bool
		if err := tx.QueryRow(ctx, "SELECT reason='[erased]' AND creation_response=jsonb_build_object('erased',true) AND receive_response IS NULL AND provider_ref IS NOT NULL FROM app.returns WHERE tenant_id=$1 AND id=$2", f.tenant, requested.ID).Scan(&clean); err != nil {
			return err
		}
		if !clean {
			t.Fatal("refund recovery lost retained authority or resurrected personal copies")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReturnReceiptReplayWaitsForErasureFence(t *testing.T) {
	for _, mode := range []string{"creation", "7e99593d-15b8-5fd9-b621-8c13cec1c290"} {
		t.Run(mode, func(t *testing.T) {
			f := newFulfillmentFixture(t)
			customer := uuid.NewString()
			err := f.maint.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,'replay-erasure@example.invalid')", f.tenant, customer); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id) VALUES($1,$2,$3)", f.tenant, customer, f.order.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			var shipped fulfillmentcontract.Shipment
			err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				shipped, err = fulfillment.New().CreateShipment(ctx, tx, f.tenant, fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: "c0787eb7-3db5-58df-a082-27a32734d147", Carrier: "manual", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: 1}}})
				return err
			})
			if err != nil || len(shipped.Lines) != 1 {
				t.Fatalf("replay shipment fixture: %+v %v", shipped, err)
			}
			request := returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: shipped.OrderVersion, IdempotencyKey: "79239faf-997b-5acd-9746-199fe6b97aa0", Kind: returnscontract.ShipmentReturn, Reason: "private replay receipt canary", Lines: []returnscontract.Line{{OrderLineID: f.line, ShipmentLineID: shipped.Lines[0].ID, Quantity: 1}}}
			var requested returnscontract.Return
			err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				requested, err = f.returns.Request(ctx, tx, f.tenant, request)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			receive := returnscontract.ReceiveInput{ReturnID: requested.ID, ExpectedVersion: requested.OrderVersion, IdempotencyKey: "a2b17012-88e5-5d39-98e7-5dfe46008f70", Restock: true}
			if mode == "7e99593d-15b8-5fd9-b621-8c13cec1c290" {
				err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.returns.Receive(ctx, tx, f.tenant, receive)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Park a successful erasure before COMMIT. READ COMMITTED replay can
			// still see the old receipt, but must wait on its owner's Order fence.
			ready, release := make(chan struct{}), make(chan struct{}, 1)
			eraser := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SELECT app.data_rights_erase_customer($1,'service',$2)", customer, uuid.NewString()); err != nil {
					return err
				}
				close(ready)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			defer eraser.stop()
			budgetBarrier(t, f, ready)
			lines, err := json.Marshal(request.Lines)
			if err != nil {
				t.Fatal(err)
			}
			var reason string
			replay := startBudgetSession(t, f, "runtime", false, func(ctx context.Context, tx pgx.Tx) error {
				if mode == "7e99593d-15b8-5fd9-b621-8c13cec1c290" {
					return tx.QueryRow(ctx, "SELECT app.returns_receive($1,$2,$3,$4)->>'reason'", receive.ReturnID, receive.ExpectedVersion, receive.IdempotencyKey, receive.Restock).Scan(&reason)
				}
				return tx.QueryRow(ctx, "SELECT app.returns_create($1,$2,$3,$4,$5,$6::jsonb)->>'reason'", request.OrderID, request.ExpectedVersion, request.IdempotencyKey, string(request.Kind), request.Reason, lines).Scan(&reason)
			})
			defer replay.stop()
			awaitBudgetBlock(t, f, replay.pid, eraser.pid)
			release <- struct{}{}
			if err := budgetDone(t, f, eraser); err != nil {
				t.Fatal(err)
			}
			if err := budgetDone(t, f, replay); err != nil {
				t.Fatal(err)
			}
			if reason != "[erased]" {
				t.Fatalf("replay returned a pre-erasure receipt after the Order fence: %q", reason)
			}
		})
	}
}

func TestFulfillmentPartialShipmentCancelRemainderRefund(t *testing.T) {
	f := newFulfillmentFixture(t)
	if got := f.stock(t); got != 16 {
		t.Fatalf("paid must commit all four units: %d", got)
	}
	in := fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: uuid.NewString(), Carrier: "manual", TrackingNumber: "gate", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: 2}}}
	var shipped fulfillmentcontract.Shipment
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		shipped, err = fulfillment.New().CreateShipment(ctx, tx, f.tenant, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped.Lines) != 1 || shipped.Lines[0].Quantity != 2 {
		t.Fatalf("shipment identity unavailable: %+v", shipped)
	}
	if got := f.stock(t); got != 16 {
		t.Fatalf("shipment deducted paid stock twice: %d", got)
	}
	var cancelled returnscontract.Return
	cancel := returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: shipped.OrderVersion, IdempotencyKey: uuid.NewString(), Kind: returnscontract.UnshippedCancel, Reason: "buyer cancelled remainder", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 2}}}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cancelled, err = f.returns.Request(ctx, tx, f.tenant, cancel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled.Restocked || gotStock(f, t) != 18 {
		t.Fatalf("remainder not restocked once: %+v", cancelled)
	}
	var replayed returnscontract.Return
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		replayed, err = f.returns.Request(ctx, tx, f.tenant, cancel)
		return err
	})
	if err != nil || replayed.ID != cancelled.ID || f.stock(t) != 18 {
		t.Fatalf("cancel replay repeated restock: %+v %v", replayed, err)
	}
	overCancel := cancel
	overCancel.IdempotencyKey = uuid.NewString()
	overCancel.ExpectedVersion = cancelled.OrderVersion
	overCancel.Lines = []returnscontract.Line{{OrderLineID: f.line, Quantity: 1}}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.returns.Request(ctx, tx, f.tenant, overCancel)
		return err
	})
	if !errors.Is(err, returnscontract.ErrConflict) {
		t.Fatalf("already allocated remainder accepted: %v", err)
	}
	f.journal.fail = true
	refund := returnscontract.RefundInput{ReturnID: cancelled.ID, ExpectedVersion: cancelled.OrderVersion, IdempotencyKey: uuid.NewString()}
	if _, err = f.returns.Refund(f.ctx, f.tenant, refund); err == nil {
		t.Fatal("injected journal failure accepted")
	}
	if len(f.payments.Events()) != 3 {
		t.Fatal("refund PSP side effect missing")
	}
	type refundResult struct {
		value returnscontract.Return
		err   error
	}
	results := make(chan refundResult, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			value, err := f.returns.Refund(f.ctx, f.tenant, refund)
			results <- refundResult{value, err}
		}()
	}
	close(start)
	var done returnscontract.Return
	for range 2 {
		result := <-results
		if result.err != nil || result.value.Status != "refunded" {
			t.Fatalf("concurrent refund recovery: %+v %v", result.value, result.err)
		}
		done = result.value
	}
	if _, err = f.returns.Refund(f.ctx, f.tenant, refund); err != nil || len(f.payments.Events()) != 3 {
		t.Fatalf("refund replay repeated PSP: %v events=%d", err, len(f.payments.Events()))
	}
	t.Run("cancel creation replay after refund", func(t *testing.T) {
		var replay returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			replay, err = f.returns.Request(ctx, tx, f.tenant, cancel)
			return err
		})
		if err != nil || !reflect.DeepEqual(replay, cancelled) {
			t.Fatalf("creation response drift: initial=%+v replay=%+v err=%v", cancelled, replay, err)
		}
	})
	var state string
	var refunded, debits, credits, journals int64
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT state,refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, f.order.ID).Scan(&state, &refunded); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT count(DISTINCT transaction_id),coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint FROM app.ledger_entries WHERE tenant_id=$1", f.tenant).Scan(&journals, &debits, &credits)
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "partially_refunded" || refunded != 230 || journals != 1 || debits != 230 || credits != 230 || f.stock(t) != 18 {
		t.Fatalf("partial cancel conservation state=%s refunded=%d journals=%d debit=%d credit=%d stock=%d", state, refunded, journals, debits, credits, f.stock(t))
	}
	// Return the two physically shipped units and refund the remaining components.
	var physical returnscontract.Return
	physicalInput := returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: done.OrderVersion, IdempotencyKey: uuid.NewString(), Kind: returnscontract.ShipmentReturn, Reason: "received damaged", Lines: []returnscontract.Line{{OrderLineID: f.line, ShipmentLineID: shipped.Lines[0].ID, Quantity: 2}}}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		physical, err = f.returns.Request(ctx, tx, f.tenant, physicalInput)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.returns.Refund(f.ctx, f.tenant, returnscontract.RefundInput{ReturnID: physical.ID, ExpectedVersion: physical.OrderVersion, IdempotencyKey: uuid.NewString()}); !errors.Is(err, returnscontract.ErrConflict) {
		t.Fatalf("refund before return receipt accepted: %v", err)
	}
	originalRequest := physical
	receiveInput := returnscontract.ReceiveInput{ReturnID: physical.ID, ExpectedVersion: physical.OrderVersion, IdempotencyKey: uuid.NewString(), Restock: true}
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		physical, err = f.returns.Receive(ctx, tx, f.tenant, receiveInput)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	originalReceive := physical
	t.Run("creation replay after receive", func(t *testing.T) {
		var replay returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			replay, err = f.returns.Request(ctx, tx, f.tenant, physicalInput)
			return err
		})
		if err != nil || !reflect.DeepEqual(replay, originalRequest) {
			t.Fatalf("creation response drift: initial=%+v replay=%+v err=%v", originalRequest, replay, err)
		}
	})
	if _, err = f.returns.Refund(f.ctx, f.tenant, returnscontract.RefundInput{ReturnID: physical.ID, ExpectedVersion: physical.OrderVersion, IdempotencyKey: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if f.stock(t) != 20 || len(f.payments.Events()) != 4 {
		t.Fatal("full return did not conserve stock/provider effects")
	}
	t.Run("creation replay after refund", func(t *testing.T) {
		var replay returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			replay, err = f.returns.Request(ctx, tx, f.tenant, physicalInput)
			return err
		})
		if err != nil || !reflect.DeepEqual(replay, originalRequest) {
			t.Fatalf("creation response drift: initial=%+v replay=%+v err=%v", originalRequest, replay, err)
		}
	})
	t.Run("receive replay after refund", func(t *testing.T) {
		var replay returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			replay, err = f.returns.Receive(ctx, tx, f.tenant, receiveInput)
			return err
		})
		if err != nil || !reflect.DeepEqual(replay, originalReceive) {
			t.Fatalf("receive response drift: initial=%+v replay=%+v err=%v", originalReceive, replay, err)
		}
	})
}
func gotStock(f fulfillmentFixture, t *testing.T) int64 { return f.stock(t) }
func TestFulfillmentReturnTenantAndQuantityFences(t *testing.T) {
	f := newFulfillmentFixture(t)
	in := fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: uuid.NewString(), Carrier: "manual", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: 5}}}
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := fulfillment.New().CreateShipment(ctx, tx, f.tenant, in)
		return err
	})
	if !errors.Is(err, fulfillmentcontract.ErrConflict) {
		t.Fatalf("over shipment: %v", err)
	}
	in.Lines[0].Quantity = 1
	err = f.runtime.InTenant(f.ctx, f.foreign, func(ctx context.Context, tx pgx.Tx) error {
		_, err := fulfillment.New().CreateShipment(ctx, tx, f.foreign, in)
		return err
	})
	if !errors.Is(err, fulfillmentcontract.ErrNotFound) {
		t.Fatalf("foreign shipment not hidden: %v", err)
	}
	in.ExpectedVersion--
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := fulfillment.New().CreateShipment(ctx, tx, f.tenant, in)
		return err
	})
	if !errors.Is(err, fulfillmentcontract.ErrPrecondition) {
		t.Fatalf("stale shipment: %v", err)
	}
	err = f.runtime.InTenant(f.ctx, f.foreign, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.returns.Receive(ctx, tx, f.foreign, returnscontract.ReceiveInput{ReturnID: uuid.NewString(), ExpectedVersion: 2, IdempotencyKey: "7e99593d-15b8-5fd9-b621-8c13cec1c290"})
		return err
	})
	if !errors.Is(err, returnscontract.ErrNotFound) {
		t.Fatalf("foreign return not hidden: %v", err)
	}
	if f.stock(t) != 16 {
		t.Fatal("rejected shipment modified paid stock")
	}
}

func TestFulfillmentReturnIdempotencyKeysAcrossRMAs(t *testing.T) {
	f := newFulfillmentFixture(t)
	var shipped fulfillmentcontract.Shipment
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		shipped, err = fulfillment.New().CreateShipment(ctx, tx, f.tenant, fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: uuid.NewString(), Carrier: "manual", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: 4}}})
		return err
	})
	if err != nil || len(shipped.Lines) != 1 {
		t.Fatalf("shipment fixture: %+v %v", shipped, err)
	}
	var first, second returnscontract.Return
	request := func(version int64) returnscontract.Return {
		t.Helper()
		var out returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: version, IdempotencyKey: uuid.NewString(), Kind: returnscontract.ShipmentReturn, Reason: "fixture", Lines: []returnscontract.Line{{OrderLineID: f.line, ShipmentLineID: shipped.Lines[0].ID, Quantity: 1}}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first = request(shipped.OrderVersion)
	second = request(first.OrderVersion)
	receiveKey := "receive:" + uuid.NewString()
	receive := func(id string, version int64, key string) (returnscontract.Return, error) {
		var out returnscontract.Return
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.returns.Receive(ctx, tx, f.tenant, returnscontract.ReceiveInput{ReturnID: id, ExpectedVersion: version, IdempotencyKey: fixtureCallerKey(key), Restock: true})
			return err
		})
		return out, err
	}
	first, err = receive(first.ID, second.OrderVersion, receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receive(second.ID, first.OrderVersion, receiveKey); !errors.Is(err, returnscontract.ErrKeyReused) {
		t.Fatalf("different RMA reused receive key: %v", err)
	}
	if f.stock(t) != 17 {
		t.Fatal("receive key collision restocked twice")
	}
	second, err = receive(second.ID, first.OrderVersion, "receive:"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	refundKey := uuid.NewString()
	first, err = f.returns.Refund(f.ctx, f.tenant, returnscontract.RefundInput{ReturnID: first.ID, ExpectedVersion: second.OrderVersion, IdempotencyKey: fixtureCallerKey(refundKey)})
	if err != nil {
		t.Fatal(err)
	}
	events := len(f.payments.Events())
	if _, err = f.returns.Refund(f.ctx, f.tenant, returnscontract.RefundInput{ReturnID: second.ID, ExpectedVersion: first.OrderVersion, IdempotencyKey: fixtureCallerKey(refundKey)}); !errors.Is(err, returnscontract.ErrKeyReused) {
		t.Fatalf("different RMA reused refund key: %v", err)
	}
	if len(f.payments.Events()) != events || f.stock(t) != 18 {
		t.Fatal("refund key collision reached a money or stock effect")
	}
}

func TestFulfillmentRefundCompletionRollbackAndReplay(t *testing.T) {
	f := newFulfillmentFixture(t)
	var cancelled returnscontract.Return
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cancelled, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: f.order.Version, IdempotencyKey: uuid.NewString(), Kind: returnscontract.UnshippedCancel, Reason: "fixture", Lines: []returnscontract.Line{{OrderLineID: f.line, Quantity: 2}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	completer, ok := f.returns.(returnscontract.RefundCompleter)
	if !ok {
		t.Fatal("trusted refund service lacks transactional completion")
	}
	completionErr := errors.New("injected completion receipt failure")
	calls := 0
	fail := true
	completion := func(ctx context.Context, tx pgx.Tx, out returnscontract.Return) error {
		calls++
		if tx == nil || out.Status != "refunded" {
			return errors.New("completion outside refund transaction")
		}
		var status, journal string
		if err := tx.QueryRow(ctx, "SELECT status,ledger_transaction_id::text FROM app.returns WHERE tenant_id=$1 AND id=$2", f.tenant, out.ID).Scan(&status, &journal); err != nil {
			return err
		}
		if status != "refunded" || journal == "" {
			return errors.New("completion did not see finalized refund")
		}
		if fail {
			return completionErr
		}
		return nil
	}
	in := returnscontract.RefundInput{ReturnID: cancelled.ID, ExpectedVersion: cancelled.OrderVersion, IdempotencyKey: uuid.NewString()}
	if _, err = completer.RefundWithCompletion(f.ctx, f.tenant, in, completion); !errors.Is(err, completionErr) {
		t.Fatalf("completion failure not propagated: %v", err)
	}
	var status string
	var refunded, journals int64
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT r.status,o.refunded_minor,(SELECT count(*) FROM app.ledger_transactions l WHERE l.tenant_id=r.tenant_id AND l.source_type='return' AND l.source_id=r.id::text) FROM app.returns r JOIN app.orders o ON o.tenant_id=r.tenant_id AND o.id=r.order_id WHERE r.tenant_id=$1 AND r.id=$2", f.tenant, cancelled.ID).Scan(&status, &refunded, &journals)
	})
	if err != nil || status != "refund_pending" || refunded != 0 || journals != 0 || len(f.payments.Events()) != 3 {
		t.Fatalf("completion error committed financial receipt: status=%s refunded=%d journals=%d events=%d err=%v", status, refunded, journals, len(f.payments.Events()), err)
	}
	fail = false
	if _, err = completer.RefundWithCompletion(f.ctx, f.tenant, in, completion); err != nil {
		t.Fatal(err)
	}
	if _, err = completer.RefundWithCompletion(f.ctx, f.tenant, in, completion); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(f.payments.Events()) != 3 {
		t.Fatalf("completed replay omitted callback or repeated PSP effect: calls=%d events=%d", calls, len(f.payments.Events()))
	}
}
