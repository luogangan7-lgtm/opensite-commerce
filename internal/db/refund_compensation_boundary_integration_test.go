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

// Commit failures, rather than function-return checks, prove both deferred sides.
func TestRefundCompensationCommitPair(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	p.receive(t, "commit-refund", payment.StatusPartiallyRefunded, 1)
	var original, source string
	run := func(fn func(context.Context, pgx.Tx) error) error {
		return p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT j.id::text FROM app.ledger_transactions j
 JOIN app.ledger_entries e ON (e.tenant_id,e.transaction_id)=(j.tenant_id,j.id)
 JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id)
 WHERE j.tenant_id=$1 AND j.source_id=$2 AND a.code='cash' AND e.side='credit'`, p.f.tenant, p.start.OrderID).Scan(&original); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id,order_id,provider_ref,amount_minor,currency,original_journal_id)
 VALUES($1,'fake',$2,'test','commit-refund',$3,$4,1,$5,$6)`, p.f.tenant, p.account, p.start.OrderID, p.start.Payment.ProviderRef, p.start.Currency, original); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT encode(sha256(convert_to(length(provider)||':'||provider||length(provider_account_id)||':'||provider_account_id
 ||length(provider_refund_id)||':'||provider_refund_id,'UTF8')),'hex') FROM app.payment_refund_attempts WHERE tenant_id=$1`, p.f.tenant).Scan(&source)
	}); err != nil {
		t.Fatal(err)
	}
	post := func(ctx context.Context, tx pgx.Tx, key string) (string, error) {
		var id string
		err := tx.QueryRow(ctx, `SELECT app.payments_post_refund_compensation($1,$2,'payment_refund',$3,'posted_to_compensated/v1',
 array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),array_agg(amount ORDER BY line_no))
 FROM app.ledger_entries WHERE tenant_id=$1 AND transaction_id=$4`, p.f.tenant, p.start.Currency, key, original).Scan(&id)
		return id, err
	}
	witness := func(ctx context.Context, tx pgx.Tx, id string) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
 VALUES($1,'fake',$2,'test','commit-refund',$3,$4)`, p.f.tenant, p.account, original, id)
		return err
	}
	for _, side := range []string{"journal", "witness"} {
		t.Run("orphan_"+side, func(t *testing.T) {
			staged := false
			err := run(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				if side == "journal" {
					_, err = post(ctx, tx, source)
				} else {
					err = witness(ctx, tx, "00000000-0000-4000-8000-000000000328")
				}
				staged = err == nil
				return err
			})
			var pg interface{ SQLState() string }
			if !staged || !errors.As(err, &pg) || (pg.SQLState() != "23514" && pg.SQLState() != "23503") {
				t.Fatalf("FIX328_COMMIT_%s_RED staged=%v: expected commit rejection, got %v", side, staged, err)
			}
		})
	}
	var reversal string
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reversal, err = post(ctx, tx, source)
		if err != nil {
			return err
		}
		return witness(ctx, tx, reversal)
	}); err != nil {
		t.Fatalf("FIX328 legitimate commit: %v", err)
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		id, err := post(ctx, tx, source)
		if err != nil || id != reversal {
			return fmt.Errorf("FIX328 replay %s != %s: %v", id, reversal, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := post(ctx, tx, source+":alias")
		return err
	}); err == nil {
		t.Fatal("FIX328_ALIAS_RED: a second compensation committed")
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		var journals, witnesses int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='payment_refund'),
 (SELECT count(*) FROM app.payment_refund_reversals WHERE tenant_id=$1)`, p.f.tenant).Scan(&journals, &witnesses); err != nil {
			return err
		}
		if journals != 1 || witnesses != 1 {
			return fmt.Errorf("FIX328 exactly once: %d journals, %d witnesses", journals, witnesses)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
