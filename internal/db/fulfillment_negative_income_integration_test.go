//go:build pgx

// Responsibilities: regress refunds when discounts exceed merchandise income.
// Dependencies: migrated pgxadapter fixture and public Orders/Fulfillment/Returns
// ports; reads immutable return components and ledger entries as witnesses.
// Callers: tagged CI gate. Invariants: positive order totals remain refundable,
// component capacities stay nonnegative, and split refunds conserve cash exactly.
package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	fulfillmentcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	orderscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

func TestRefundNegativeAdjustedIncomeCapacity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chunks   []int64
		physical bool
		legacy   int64
	}{
		{name: "cancelFull", chunks: []int64{2}},
		{name: "cancelSplit", chunks: []int64{1, 1}},
		{name: "returnFull", chunks: []int64{2}, physical: true},
		{name: "returnSplit", chunks: []int64{1, 1}, physical: true},
		{name: "cancelSplitAfterLegacy", chunks: []int64{1, 1}, legacy: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Independent arithmetic: 2*50 merchandise +60 shipping -150
			// discount =10 paid. The excess discount consumes 50 shipping;
			// every refund therefore debits shipping, never negative income.
			f, initialStock := newConservationFixture(t, 2, 50, 0, 60, 150)
			if f.order.Total.Amount != 10 {
				t.Fatalf("fixture total=%d, want 10", f.order.Total.Amount)
			}
			if tc.legacy > 0 {
				err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					_, err := orders.New().Transition(ctx, tx, f.tenant, orderscontract.TransitionInput{OrderID: f.order.ID, Target: orderscontract.PartiallyRefunded, EventKey: "negative-income-legacy", RefundMinor: tc.legacy})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			shipmentLine := ""
			if tc.physical {
				version, _, _ := conservationOrder(t, f)
				err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					s, err := fulfillment.New().CreateShipment(ctx, tx, f.tenant, fulfillmentcontract.CreateInput{OrderID: f.order.ID, ExpectedVersion: version, IdempotencyKey: "negative-income-shipment", Carrier: "manual", TrackingNumber: "negative-income", Lines: []fulfillmentcontract.Line{{OrderLineID: f.line, Quantity: 2}}})
					if err == nil {
						shipmentLine = s.Lines[0].ID
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			var frozenTotal int64
			for i, quantity := range tc.chunks {
				version, _, _ := conservationOrder(t, f)
				kind := returnscontract.UnshippedCancel
				if tc.physical {
					kind = returnscontract.ShipmentReturn
				}
				var rma returnscontract.Return
				err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					rma, err = f.returns.Request(ctx, tx, f.tenant, returnscontract.RequestInput{OrderID: f.order.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("negative-income-rma-%d", i), Kind: kind, Reason: "discount exceeds merchandise income", Lines: []returnscontract.Line{{OrderLineID: f.line, ShipmentLineID: shipmentLine, Quantity: quantity}}})
					return err
				})
				if err != nil {
					t.Fatalf("positive-total order must permit refund request: %v", err)
				}
				var income, tax, shipping, lineIncome, lineTax, lineShipping int64
				err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT r.income_minor,r.tax_minor,r.shipping_minor,
					 sum(l.income_minor)::bigint,sum(l.tax_minor)::bigint,sum(l.shipping_minor)::bigint
					 FROM app.returns r JOIN app.return_lines l ON l.tenant_id=r.tenant_id AND l.return_id=r.id
					 WHERE r.tenant_id=$1 AND r.id=$2 GROUP BY r.id,r.tenant_id`, f.tenant, rma.ID).Scan(&income, &tax, &shipping, &lineIncome, &lineTax, &lineShipping)
				})
				if err != nil {
					t.Fatal(err)
				}
				want := int64(10) - tc.legacy
				if len(tc.chunks) == 2 {
					want = []int64{(10 - tc.legacy) / 2, (10 - tc.legacy) - (10-tc.legacy)/2}[i]
				}
				if income != 0 || tax != 0 || shipping != want || lineIncome != 0 || lineTax != 0 || lineShipping != want || rma.RefundMinor != want {
					t.Fatalf("component capacity: return=(%d,%d,%d) lines=(%d,%d,%d) refund=%d want shipping=%d", income, tax, shipping, lineIncome, lineTax, lineShipping, rma.RefundMinor, want)
				}
				frozenTotal += rma.RefundMinor
				if tc.physical {
					version, _, _ = conservationOrder(t, f)
					err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
						var err error
						rma, err = f.returns.Receive(ctx, tx, f.tenant, returnscontract.ReceiveInput{ReturnID: rma.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("negative-income-receive-%d", i), Restock: true})
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				version, _, _ = conservationOrder(t, f)
				out, err := f.returns.Refund(f.ctx, f.tenant, returnscontract.RefundInput{ReturnID: rma.ID, ExpectedVersion: version, IdempotencyKey: fmt.Sprintf("negative-income-refund-%d", i)})
				if err != nil || out.Status != "refunded" || out.RefundMinor != want {
					t.Fatalf("refund completion: %+v err=%v", out, err)
				}
			}
			_, refunded, state := conservationOrder(t, f)
			if frozenTotal+tc.legacy != 10 || refunded != 10 || state != "refunded" || f.stock(t) != initialStock {
				t.Fatalf("conservation: frozen=%d legacy=%d refunded=%d state=%s stock=%d want=%d", frozenTotal, tc.legacy, refunded, state, f.stock(t), initialStock)
			}
			var journals, salesDebits, taxDebits, shippingDebits, cashCredits, unbalanced int64
			err := f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(DISTINCT e.transaction_id),
				 coalesce(sum(e.amount) FILTER(WHERE a.code='sales' AND e.side='debit'),0)::bigint,
				 coalesce(sum(e.amount) FILTER(WHERE a.code='tax' AND e.side='debit'),0)::bigint,
				 coalesce(sum(e.amount) FILTER(WHERE a.code='shipping' AND e.side='debit'),0)::bigint,
				 coalesce(sum(e.amount) FILTER(WHERE a.code='cash' AND e.side='credit'),0)::bigint,
				 (SELECT count(*) FROM (SELECT le.transaction_id FROM app.ledger_entries le
				 JOIN app.returns rr ON rr.tenant_id=le.tenant_id AND rr.ledger_transaction_id=le.transaction_id
				 WHERE rr.tenant_id=$1 AND rr.order_id=$2 GROUP BY le.transaction_id
				 HAVING sum(CASE le.side WHEN 'debit' THEN le.amount ELSE -le.amount END)<>0) bad)
				 FROM app.ledger_entries e JOIN app.ledger_accounts a ON a.tenant_id=e.tenant_id AND a.id=e.account_id
				 JOIN app.returns r ON r.tenant_id=e.tenant_id AND r.ledger_transaction_id=e.transaction_id
				 WHERE r.tenant_id=$1 AND r.order_id=$2`, f.tenant, f.order.ID).Scan(&journals, &salesDebits, &taxDebits, &shippingDebits, &cashCredits, &unbalanced)
			})
			if err != nil {
				t.Fatal(err)
			}
			if journals != int64(len(tc.chunks)) || salesDebits != 0 || taxDebits != 0 || shippingDebits != 10-tc.legacy || cashCredits != 10-tc.legacy || unbalanced != 0 || len(f.payments.Events()) != len(tc.chunks)+2 {
				t.Fatalf("ledger: journals=%d sales=%d tax=%d shipping=%d cash=%d unbalanced=%d PSP=%d", journals, salesDebits, taxDebits, shippingDebits, cashCredits, unbalanced, len(f.payments.Events()))
			}
		})
	}
}
