//go:build pgx

// Responsibilities: regress legacy/RMA conservation and zero-value completion.
// Dependencies: existing fulfillment fixture, public Orders/Fulfillment/Returns
// ports and tenanttx; reads Orders, Returns, Inventory and Ledger as witnesses.
// Callers: tagged CI gate. Invariants: frozen refunds conserve remaining credit,
// each RMA restores once, zero returns finish without PSP or ledger side effects.
package db_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	fulfillmentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

// Reuse the migrated-DB fixture and its mandatory pgxtest.Skip policy. Its first
// order remains isolated from the second order's money and RMA witnesses below.
func newConservationFixture(t *testing.T, quantity, unit, taxBasisPoints, shippingMinor, discount int64) (fulfillmentFixture, int64) {
	t.Helper()
	f := newFulfillmentFixture(t)
	initialStock := f.stock(t)
	f.line = uuid.NewString()
	net := quantity * unit
	taxMinor := (net*taxBasisPoints + 5000) / 10000
	zone, rate := uuid.NewString(), uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := orderscontract.CreateInput{
		ID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Currency: "USD",
		Buyer:    orderscontract.Buyer{Guest: orderscontract.Contact{Email: "conservation@example.test"}},
		Shipping: shipping.Result{Charge: money.Money{Amount: shippingMinor, Currency: "USD"}, ZoneID: zone, RateID: rate, Method: "fixed"},
		Lines: []orderscontract.Line{{ID: f.line, SKUID: f.sku, SKU: "FULFILLMENT-SKU", Quantity: quantity,
			Price: pricing.Result{Unit: money.Money{Amount: unit, Currency: "USD"}, Line: money.Money{Amount: net, Currency: "USD"}, PriceListID: uuid.NewString(), MinQuantity: 1},
			Tax:   tax.LineResult{ID: f.line, Amounts: tax.Amounts{Net: net, Tax: taxMinor, Gross: net + taxMinor}, Snapshot: tax.Snapshot{Currency: "USD", ZoneID: zone, RateID: rate, Category: "standard", BasisPoints: taxBasisPoints, ValidFrom: &at, Rounding: "half_up", Reason: "rate"}},
		}},
	}
	if discount != 0 {
		in.Adjustments = []orderscontract.Adjustment{{ID: uuid.NewString(), Kind: "discount", AmountMinor: -discount, Reason: "conservation fixture"}}
	}
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.order, err = orders.New().Create(ctx, tx, f.tenant, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.order.Total.Amount != net+taxMinor+shippingMinor-discount {
		t.Fatal("fixture total differs from independent arithmetic")
	}
	f.payments = paymentfake.New()
	req := payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: f.order.ID, OrderVersion: f.order.Version, Amount: f.order.Total, IdempotencyKey: uuid.NewString()}
	intent, err := f.payments.CreateIntent(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = uuid.NewString()
	if _, err := f.payments.Capture(f.ctx, payment.PaymentOperation{CreatePaymentIntent: req, ProviderRef: intent.ProviderRef}); err != nil {
		t.Fatal(err)
	}
	var accounts returnscontract.RefundAccounts
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.order, err = orders.New().Transition(ctx, tx, f.tenant, orderscontract.TransitionInput{OrderID: f.order.ID, Target: orderscontract.Paid, EventKey: uuid.NewString()})
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='sales'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='tax'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='shipping'),
		 (SELECT id::text FROM app.ledger_accounts WHERE tenant_id=$1 AND code='cash')`, f.tenant).Scan(&accounts.Income, &accounts.Tax, &accounts.Shipping, &accounts.Cash)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.returns = returns.New(returns.Config{Runner: f.service, Payments: f.payments,
		Captures: fulfillmentCapture{captured: returnscontract.CapturedPayment{TenantID: f.tenant, OrderID: f.order.ID, ProviderRef: intent.ProviderRef, OrderVersion: req.OrderVersion, Paid: req.Amount}},
		Accounts: fulfillmentAccounts{tenant: f.tenant, accounts: accounts}, Ledger: f.journal})
	return f, initialStock
}

func conservationOrder(t *testing.T, f fulfillmentFixture) (int64, int64, string) {
	t.Helper()
	var version, refunded int64
	var state string
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT version,refunded_minor,state FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, f.order.ID).Scan(&version, &refunded, &state)
	})
	if err != nil {
		t.Fatal(err)
	}
	return version, refunded, state
}

func runRefundConservation(t *testing.T, quantity, unit, taxBasisPoints, shippingMinor, discount, legacy int64, chunks []int64, sequence []int, physical bool) {
	t.Helper()
	f, initialStock := newConservationFixture(t, quantity, unit, taxBasisPoints, shippingMinor, discount)
	taxMinor := (quantity*unit*taxBasisPoints + 5000) / 10000
	total := quantity*unit + taxMinor + shippingMinor - discount
	if legacy > 0 {
		target := orderscontract.PartiallyRefunded
		if legacy == total {
			target = orderscontract.Refunded
		}
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orders.New().Transition(ctx, tx, f.tenant, orderscontract.TransitionInput{OrderID: f.order.ID, Target: target, EventKey: "legacy-credit", RefundMinor: legacy})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	shipmentLine := ""
	if physical {
		version, _, _ := conservationOrder(t, f)
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			s, err := fulfillment.New().CreateShipment(ctx, tx, f.tenant, fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: version, IdempotencyKey: "ship-all", Carrier: "manual", TrackingNumber: "conservation", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: quantity}}})
			if err == nil {
				shipmentLine = s.Lines[0].ID
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rmas := make([]returnscontract.Return, len(chunks))
	var frozenSum int64
	for i, chunk := range chunks {
		version, _, _ := conservationOrder(t, f)
		kind := returnscontract.UnshippedCancel
		if physical {
			kind = returnscontract.ShipmentReturn
		}
		in := returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("rma-%d", i), Kind: kind, Reason: "conservation", Lines: []returnscontract.Line{{OrderLineID: f.line, ShipmentLineID: shipmentLine, Quantity: chunk}}}
		err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rmas[i], err = f.returns.Request(ctx, tx, f.tenant, in)
			return err
		})
		if err != nil {
			t.Fatalf("request chunk %d: %v", i, err)
		}
		if rmas[i].RefundMinor < 0 {
			t.Fatal("negative frozen entitlement")
		}
		frozenSum += rmas[i].RefundMinor
		if frozenSum > total-legacy {
			t.Fatalf("legacy=%d frozen=%d exceeds remaining=%d", legacy, frozenSum, total-legacy)
		}
		if !physical && rmas[i].RefundMinor == 0 && rmas[i].Status != "completed" {
			t.Fatalf("zero cancellation stranded: %+v", rmas[i])
		}
		if legacy == total {
			_, refunded, state := conservationOrder(t, f)
			if refunded != total || state != "refunded" {
				t.Fatalf("zero cancellation changed fully refunded order: amount=%d state=%s", refunded, state)
			}
		}
	}
	if frozenSum+legacy != total {
		t.Fatalf("frozen conservation: legacy %d + RMA %d != total %d", legacy, frozenSum, total)
	}
	for _, i := range sequence {
		rma := rmas[i]
		if physical {
			version, _, _ := conservationOrder(t, f)
			in := returnscontract.ReceiveInput{ReturnID: rma.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("receive-%d", i), Restock: true}
			err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				rma, err = f.returns.Receive(ctx, tx, f.tenant, in)
				return err
			})
			if err != nil {
				t.Fatalf("receive chunk %d (frozen=%d): %v", i, rmas[i].RefundMinor, err)
			}
			before := f.stock(t)
			err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				replay, err := f.returns.Receive(ctx, tx, f.tenant, in)
				if err == nil && (replay.ID != rma.ID || replay.Status != rma.Status || replay.RefundMinor != rma.RefundMinor) {
					return fmt.Errorf("receive replay drift: %+v", replay)
				}
				return err
			})
			if err != nil || f.stock(t) != before {
				t.Fatalf("receive replay restored twice: %v", err)
			}
		}
		if rma.RefundMinor == 0 {
			if rma.Status != "completed" || !rma.Restocked {
				t.Fatalf("zero return not terminal/restocked: %+v", rma)
			}
			continue
		}
		version, _, _ := conservationOrder(t, f)
		in := returnscontract.RefundInput{ReturnID: rma.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("refund-%d", i)}
		refunded, err := f.returns.Refund(f.ctx, f.tenant, in)
		if err != nil {
			t.Fatalf("refund chunk %d: %v", i, err)
		}
		if refunded.Status != "refunded" || refunded.RefundMinor != rmas[i].RefundMinor {
			t.Fatalf("frozen amount changed on completion: %+v", refunded)
		}
		events := len(f.payments.Events())
		if _, err := f.returns.Refund(f.ctx, f.tenant, in); err != nil || len(f.payments.Events()) != events {
			t.Fatalf("refund replay repeated PSP: %v", err)
		}
		_, amount, _ := conservationOrder(t, f)
		if amount > total {
			t.Fatalf("order over-refunded: %d > %d", amount, total)
		}
	}
	_, refunded, state := conservationOrder(t, f)
	if refunded != total || state != "refunded" || f.stock(t) != initialStock {
		t.Fatalf("terminal money/stock: refunded=%d total=%d state=%s stock=%d want=%d", refunded, total, state, f.stock(t), initialStock)
	}
	var movements, journals, zeroFinancial, unbalanced, ledgerCredits int64
	err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM app.inventory_movements m JOIN app.returns r ON r.tenant_id=m.tenant_id AND r.id=m.owner_id WHERE r.tenant_id=$1 AND r.order_id=$2 AND m.owner_type='return'),
		 (SELECT count(*) FROM app.returns WHERE tenant_id=$1 AND order_id=$2 AND ledger_transaction_id IS NOT NULL),
		 (SELECT count(*) FROM app.returns WHERE tenant_id=$1 AND order_id=$2 AND refund_minor=0 AND (status<>'completed' OR refund_key IS NOT NULL OR ledger_transaction_id IS NOT NULL)),
		 (SELECT count(*) FROM (SELECT e.transaction_id FROM app.ledger_entries e JOIN app.returns r ON r.tenant_id=e.tenant_id AND r.ledger_transaction_id=e.transaction_id WHERE r.tenant_id=$1 AND r.order_id=$2 GROUP BY e.transaction_id HAVING sum(CASE e.side WHEN 'debit' THEN e.amount ELSE -e.amount END)<>0) bad),
		 (SELECT coalesce(sum(e.amount),0)::bigint FROM app.ledger_entries e JOIN app.returns r ON r.tenant_id=e.tenant_id AND r.ledger_transaction_id=e.transaction_id WHERE r.tenant_id=$1 AND r.order_id=$2 AND e.side='credit')`, f.tenant, f.order.ID).Scan(&movements, &journals, &zeroFinancial, &unbalanced, &ledgerCredits)
	})
	if err != nil {
		t.Fatal(err)
	}
	positive := int64(0)
	for _, rma := range rmas {
		if rma.RefundMinor > 0 {
			positive++
		}
	}
	if movements != int64(len(chunks)) || journals != positive || zeroFinancial != 0 || unbalanced != 0 || ledgerCredits != total-legacy || int64(len(f.payments.Events())) != positive+2 {
		t.Fatalf("receipt witnesses: movements=%d journals=%d zeroFinancial=%d unbalanced=%d credits=%d PSP events=%d", movements, journals, zeroFinancial, unbalanced, ledgerCredits, len(f.payments.Events()))
	}
}

func TestRefundConservationLegacyCredit(t *testing.T) {
	t.Run("full190", func(t *testing.T) { runRefundConservation(t, 2, 100, 0, 0, 0, 10, []int64{2}, []int{0}, false) })
	t.Run("split95and95", func(t *testing.T) { runRefundConservation(t, 2, 100, 0, 0, 0, 10, []int64{1, 1}, []int{1, 0}, false) })
	t.Run("fullyCreditedCancelCompletesZero", func(t *testing.T) { runRefundConservation(t, 2, 100, 0, 0, 0, 200, []int64{2}, []int{0}, false) })
}

func TestRefundConservationZeroReceiveAfterRefunded(t *testing.T) {
	runRefundConservation(t, 2, 1, 0, 0, 1, 0, []int64{1, 1}, []int{1, 0}, true)
}

func TestRefundConservationSeeded150Orders(t *testing.T) {
	const seed int64 = 39_20261013
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 150; i++ {
		quantity := int64(2 + rng.Intn(8))
		unit := int64(1 + rng.Intn(100))
		taxBasisPoints, shippingMinor := int64(rng.Intn(2501)), int64(rng.Intn(31))
		taxMinor := (quantity*unit*taxBasisPoints + 5000) / 10000
		discount := int64(rng.Intn(int(quantity * unit)))
		total := quantity*unit + taxMinor + shippingMinor - discount
		legacy := rng.Int63n(total)
		var chunks []int64
		for remaining := quantity; remaining > 0; {
			chunk := int64(1 + rng.Intn(int(remaining)))
			chunks = append(chunks, chunk)
			remaining -= chunk
		}
		sequence, physical := rng.Perm(len(chunks)), rng.Intn(2) == 0
		// Exercise full legacy credit only for unshipped cancellation: a fully
		// refunded order cannot start a new shipment, but still owns paid stock.
		if !physical && rng.Intn(4) == 0 {
			legacy = total
		}
		t.Run(fmt.Sprintf("seed%d-case%03d", seed, i), func(t *testing.T) {
			t.Logf("qty=%d unit=%d tax=%d shipping=%d discount=%d legacy=%d chunks=%v sequence=%v physical=%t", quantity, unit, taxMinor, shippingMinor, discount, legacy, chunks, sequence, physical)
			runRefundConservation(t, quantity, unit, taxBasisPoints, shippingMinor, discount, legacy, chunks, sequence, physical)
		})
	}
}
