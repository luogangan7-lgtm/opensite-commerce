// Responsibility: server consent regression witnesses against real Checkout PG.
// Dependencies: checkout PG fixture and public contract; caller: foundation CI.
// Invariants: no order/provider effect without matching explicit accepted money.
package db_test

import (
	"encoding/json"
	"context"
	"github.com/jackc/pgx/v5"
	"testing"

	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

func TestCheckoutPGChangedTotalConsent(t *testing.T) {
	for _, accepted := range []string{`null`, `{"amount_minor":159,"currency":"USD"}`, `{"amount_minor":160,"currency":"EUR"}`} {
		t.Run(accepted, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 5, 1)
			input := f.input()
			raw, _ := json.Marshal(input)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			fields["accepted_total"] = json.RawMessage(accepted)
			raw, _ = json.Marshal(fields)
			var in checkoutcontract.StartInput
			if err := json.Unmarshal(raw, &in); err != nil { t.Fatal(err) }
			if _, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in); err == nil {
				t.Fatal("Start accepted missing/stale/currency-mismatched buyer consent")
			}
			var count int
			if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, "SELECT count(*) FROM app.orders WHERE tenant_id=$1", f.tenant).Scan(&count) }); err != nil || count != 0 { t.Fatalf("rejected consent created order: %d %v", count, err) }
		})
	}
}
