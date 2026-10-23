//go:build pgx

// Responsibility: real-PG fences for immutable compensation identities.
// Dependencies: migrated Checkout/Payments fixtures and Ledger public command.
// Caller: CI pgx lane; these privileged fixtures do not enable a runtime finalizer.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	paymentscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
)

func TestRefundCompensationIdentityFences(t *testing.T) {
	p := newPaymentFactFixture(t)
	p.receive(t, "capture", payment.StatusCaptured, p.start.AmountMinor)
	p.receive(t, "compensable", payment.StatusPartiallyRefunded, 1)
	before := p.witness(t)
	err := p.f.runner.InMaintenance(p.f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", p.f.tenant); err != nil {
			return err
		}
		var original string
		if err := tx.QueryRow(ctx, `SELECT j.id::text FROM app.ledger_transactions j
   JOIN app.ledger_entries e ON (e.tenant_id,e.transaction_id)=(j.tenant_id,j.id)
   JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id)
   WHERE j.tenant_id=$1 AND j.source_id=$2 AND a.code='cash' AND e.side='credit'`, p.f.tenant, p.start.OrderID).Scan(&original); err != nil {
			return err
		}
		insert := `INSERT INTO app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id,order_id,provider_ref,amount_minor,currency,original_journal_id)
   VALUES($1,'fake',$2,'test','compensable',$3,$4,1,$5,$6)`
		args := []any{p.f.tenant, p.account, p.start.OrderID, p.start.Payment.ProviderRef, p.start.Currency, original}
		if _, err := tx.Exec(ctx, insert, args...); err != nil {
			return err
		}
		reject := func(query, code string, args ...any) error {
			nested, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			defer nested.Rollback(ctx)
			_, err = nested.Exec(ctx, query, args...)
			var pg interface{ SQLState() string }
			if !errors.As(err, &pg) || pg.SQLState() != code {
				return fmt.Errorf("C19D expected %s, got %v", code, err)
			}
			return nil
		}
		if err := reject(insert, "23505", args...); err != nil {
			return err
		}
		if err := reject("UPDATE app.payment_refund_attempts SET amount_minor=2 WHERE tenant_id=$1", "23514", p.f.tenant); err != nil {
			return err
		}
		if err := reject("DELETE FROM app.payment_refund_attempts WHERE tenant_id=$1", "23514", p.f.tenant); err != nil {
			return err
		}
		// Valid FK is insufficient: only the exact original refund journal is a witness.
		if err := reject(`INSERT INTO app.payment_refund_attempts SELECT tenant_id,provider,provider_account_id,environment,'alias',order_id,provider_ref,amount_minor,currency,return_id,attempt_key,original_journal_id,created_at FROM app.payment_refund_attempts WHERE tenant_id=$1`, "23514", p.f.tenant); err != nil {
			return err
		}
		var reversal string
		source, err := (paymentscontract.RefundIdentity{TenantID: p.f.tenant, Provider: "fake", AccountID: p.account, RefundID: "compensable"}).ReversalSourceID()
		if err != nil {
			return err
		}
		// A balanced, posted journal with the correct source is still insufficient
		// when its sides do not reverse the original. The savepoint rolls it back.
		badWitness := `WITH bad AS (
 SELECT app.post_ledger($1,$3,'payment_refund',$2,'posted_to_compensated/v1',
 array_agg(account_id ORDER BY line_no),array_agg(side ORDER BY line_no),array_agg(amount ORDER BY line_no)) AS id
 FROM app.ledger_entries WHERE tenant_id=$1 AND transaction_id=$4)
 INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
 SELECT $1,'fake',$5,'test','compensable',$4,id FROM bad`
		if err := reject(badWitness, "23514", p.f.tenant, source, p.start.Currency, original, p.account); err != nil {
			return err
		}
		query := `SELECT app.post_ledger($1,$3,'payment_refund',$2,'posted_to_compensated/v1',
   array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),array_agg(amount ORDER BY line_no))
   FROM app.ledger_entries WHERE tenant_id=$1 AND transaction_id=$4`
		if err := tx.QueryRow(ctx, query, p.f.tenant, source, p.start.Currency, original).Scan(&reversal); err != nil {
			return err
		}
		witness := `INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
   VALUES($1,'fake',$2,'test','compensable',$3,$4)`
		if err := reject(witness, "23514", p.f.tenant, p.account, original, original); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, witness, p.f.tenant, p.account, original, reversal); err != nil {
			return err
		}
		var replay string
		if err := tx.QueryRow(ctx, query, p.f.tenant, source, p.start.Currency, original).Scan(&replay); err != nil || replay != reversal {
			return fmt.Errorf("C19D replay journal: %s %v", replay, err)
		}
		// Try to post the reversal again under an alias and commit its witness.
		// The SQL proof rejects it and the savepoint removes the second journal.
		secondReversal := `WITH extra AS (
 SELECT app.post_ledger($1,$3,'payment_refund',$2||':duplicate','posted_to_compensated/v1',
 array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),array_agg(amount ORDER BY line_no)) AS id
 FROM app.ledger_entries WHERE tenant_id=$1 AND transaction_id=$4)
 INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
 SELECT $1,'fake',$5,'test','compensable',$4,id FROM extra`
		if err := reject(secondReversal, "23514", p.f.tenant, source, p.start.Currency, original, p.account); err != nil {
			return fmt.Errorf("C19D_SECOND_REVERSAL_RED: %w", err)
		}
		if err := reject(witness, "23505", p.f.tenant, p.account, original, reversal); err != nil {
			return err
		}
		if err := reject("UPDATE app.payment_refund_reversals SET reversal_journal_id=original_journal_id WHERE tenant_id=$1", "23514", p.f.tenant); err != nil {
			return err
		}
		if err := reject("SELECT app.orders_transition($1,'paid','generic-compensation',-1)", "22023", p.start.OrderID); err != nil {
			return fmt.Errorf("C19D_GENERIC_DECREMENT_RED: %w", err)
		}
		if err := reject("UPDATE app.orders SET refunded_minor=0,state='paid',version=version+1 WHERE tenant_id=$1 AND id=$2", "55000", p.f.tenant, p.start.OrderID); err != nil {
			return err
		}
		if err := reject("TRUNCATE app.payment_refund_attempts CASCADE", "23514"); err != nil {
			return err
		}
		if err := reject("TRUNCATE app.payment_refund_reversals", "23514"); err != nil {
			return err
		}
		// Give the fixture only SELECT in a savepoint to exercise FORCE RLS itself,
		// independently from the production capability's complete ACL denial.
		rls, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer rls.Rollback(ctx)
		if _, err = rls.Exec(ctx, "GRANT SELECT ON app.payment_refund_attempts TO app_payments_service"); err != nil {
			return err
		}
		if _, err = rls.Exec(ctx, "SET LOCAL ROLE app_payments_service"); err != nil {
			return err
		}
		for _, scope := range []struct {
			tenant string
			count  int
		}{{p.f.tenant, 1}, {"00000000-0000-0000-0000-000000000001", 0}, {"", 0}} {
			if _, err = rls.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", scope.tenant); err != nil {
				return err
			}
			var count int
			if err = rls.QueryRow(ctx, "SELECT count(*) FROM app.payment_refund_attempts").Scan(&count); err != nil {
				return err
			}
			if count != scope.count {
				return fmt.Errorf("C19D RLS count %d want %d", count, scope.count)
			}
		}
		if err = rls.Rollback(ctx); err != nil {
			return err
		}
		// Always roll privileged fixture money back: preparation must have no financial effect.
		return errors.New("C19D fixture rollback")
	})
	if err == nil || err.Error() != "C19D fixture rollback" {
		t.Fatal(err)
	}
	if after := p.witness(t); after != before {
		t.Fatalf("preparation changed money: before=%+v after=%+v", before, after)
	}
}
