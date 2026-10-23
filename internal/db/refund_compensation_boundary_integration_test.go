//go:build pgx

// Responsibility: reject compensation through every generic Ledger caller.
// Dependencies: real migrated PostgreSQL and successful Checkout refund facts.
// Caller: CI pgx lane; invariant: application roles cannot mint reversals.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestRefundCompensationLedgerBoundary(t *testing.T) {
	for _, role := range []string{"app_runtime", "app_returns_service"} {
		t.Run(role, func(t *testing.T) {
			p := newPaymentFactFixture(t)
			p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
			p.receive(t, "boundary-refund", payment.StatusPartiallyRefunded, 1)
			err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
					return err
				}
				var accounts, sides []string
				var amounts []int64
				if err := tx.QueryRow(ctx, `SELECT array_agg(e.account_id::text ORDER BY e.line_no),
 array_agg(CASE e.side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY e.line_no),array_agg(e.amount ORDER BY e.line_no)
 FROM app.ledger_entries e JOIN app.ledger_transactions j ON (j.tenant_id,j.id)=(e.tenant_id,e.transaction_id)
 WHERE j.tenant_id=$1 AND j.source_id=$2 AND EXISTS(SELECT FROM app.ledger_entries x JOIN app.ledger_accounts a
 ON (a.tenant_id,a.id)=(x.tenant_id,x.account_id) WHERE x.tenant_id=j.tenant_id AND x.transaction_id=j.id AND a.code='cash' AND x.side='credit')`, p.f.tenant, p.start.OrderID).Scan(&accounts, &sides, &amounts); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SELECT app.post_ledger($1,$2,'payment_refund','unwitnessed-alias','posted_to_compensated/v1',$3::uuid[],$4::text[],$5::bigint[])`, p.f.tenant, p.start.Currency, accounts, sides, amounts)
				var pg interface{ SQLState() string }
				if !errors.As(err, &pg) || pg.SQLState() != "42501" {
					return fmt.Errorf("FIX328_ROLE_RED %s: expected 42501, got %v", role, err)
				}
				return errors.New("FIX328 expected rollback")
			})
			if err == nil || err.Error() != "FIX328 expected rollback" {
				t.Fatal(err)
			}
		})
	}
}
