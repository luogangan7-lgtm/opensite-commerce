//go:build pgx

// Responsibility: A-only disposable DB clock mapping and zero-journal witness.
// Dependencies: existing PG fixture and live probe; callers: fake CI and owner entry.
// No Checkout call, financial applier, Inbox, live fact or provider ID is persisted.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

func liveSmokeFixture(t *testing.T, o payments.LiveSmokeOptions) (*checkoutPGFixture, *payments.StripeLiveSmoke) {
	t.Helper()
	f := newCheckoutPGFixture(t, 100, 45)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	f.ctx = ctx
	adapter, err := payments.NewStripeLiveSmoke(payments.Config{
		Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
			if account != o.Account {
				return errors.New("unmapped probe account")
			}
			return f.run(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				var at time.Time
				if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at); err != nil {
					return err
				}
				return fn(ctx, tx, f.tenant, at)
			})
		},
	}, o)
	if err != nil {
		t.Fatal("live adapter admission failed")
	}
	return f, adapter
}

func liveSmokeWitness(t *testing.T, f *checkoutPGFixture) {
	t.Helper()
	var journals, facts, accounts, orders int
	err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_events WHERE tenant_id=$1),
 (SELECT count(*) FROM app.payment_provider_accounts WHERE tenant_id=$1),
 (SELECT count(*) FROM app.orders WHERE tenant_id=$1)`, f.tenant).Scan(&journals, &facts, &accounts, &orders)
	})
	if err != nil || journals != 0 || facts != 0 || accounts != 0 || orders != 0 {
		t.Fatal("A-only probe produced financial state")
	}
}

func emitLiveSummary(v payments.LiveSmokeSummary) {
	b, _ := json.Marshal(v)
	fmt.Println("OSC_LIVE_SUMMARY " + string(b))
}
