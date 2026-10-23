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
 "github.com/jackc/pgx/v5/pgconn"
 payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

func TestRefundCompensationIdentityFences(t *testing.T) {
 p := newPaymentFactFixture(t)
 p.receive(t,"capture",payment.StatusCaptured,p.start.AmountMinor)
 p.receive(t,"compensable",payment.StatusPartiallyRefunded,1)
 before := p.witness(t)
 err := p.f.runner.InMaintenance(p.f.ctx,func(ctx context.Context,tx pgx.Tx) error {
  if _,err:=tx.Exec(ctx,"SELECT set_config('app.tenant_id',$1,true)",p.f.tenant);err!=nil{return err}
  var original string
  if err:=tx.QueryRow(ctx,`SELECT j.id::text FROM app.ledger_transactions j
   JOIN app.ledger_entries e ON (e.tenant_id,e.transaction_id)=(j.tenant_id,j.id)
   JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id)
   WHERE j.tenant_id=$1 AND j.source_id=$2 AND a.code='cash' AND e.side='credit'`,p.f.tenant,p.start.OrderID).Scan(&original);err!=nil{return err}
  insert:=`INSERT INTO app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id,order_id,provider_ref,amount_minor,currency,original_journal_id)
   VALUES($1,'fake',$2,'test','compensable',$3,$4,1,$5,$6)`
  args:=[]any{p.f.tenant,p.account,p.start.OrderID,p.start.Payment.ProviderRef,p.start.Currency,original}
  if _,err:=tx.Exec(ctx,insert,args...);err!=nil{return err}
  reject:=func(query,code string,args ...any) error {
   nested,err:=tx.Begin(ctx);if err!=nil{return err};defer nested.Rollback(ctx)
   _,err=nested.Exec(ctx,query,args...)
   var pg *pgconn.PgError
   if !errors.As(err,&pg)||pg.Code!=code{return fmt.Errorf("C19D expected %s, got %v",code,err)}
   return nil
  }
  if err:=reject(insert,"23505",args...);err!=nil{return err}
  if err:=reject("UPDATE app.payment_refund_attempts SET amount_minor=2 WHERE tenant_id=$1","23514",p.f.tenant);err!=nil{return err}
  if err:=reject("DELETE FROM app.payment_refund_attempts WHERE tenant_id=$1","23514",p.f.tenant);err!=nil{return err}
  // Valid FK is insufficient: only the exact original refund journal is a witness.
  if err:=reject(`INSERT INTO app.payment_refund_attempts SELECT tenant_id,provider,provider_account_id,environment,'alias',order_id,provider_ref,amount_minor,currency,return_id,attempt_key,original_journal_id,created_at FROM app.payment_refund_attempts WHERE tenant_id=$1`,"23514",p.f.tenant);err!=nil{return err}
  var reversal string
  source:=`encode(sha256(convert_to('4:fake'||length($2::text)||':'||$2||'11:compensable','UTF8')),'hex')`
  query:=`SELECT app.post_ledger($1,$3,'payment_refund',`+source+`,'posted_to_compensated/v1',
   array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),array_agg(amount ORDER BY line_no))
   FROM app.ledger_entries WHERE tenant_id=$1 AND transaction_id=$4`
  if err:=tx.QueryRow(ctx,query,p.f.tenant,p.account,p.start.Currency,original).Scan(&reversal);err!=nil{return err}
  witness:=`INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
   VALUES($1,'fake',$2,'test','compensable',$3,$4)`
  if _,err:=tx.Exec(ctx,witness,p.f.tenant,p.account,original,reversal);err!=nil{return err}
  if err:=reject(witness,"23505",p.f.tenant,p.account,original,reversal);err!=nil{return err}
  if err:=reject("UPDATE app.payment_refund_reversals SET reversal_journal_id=original_journal_id WHERE tenant_id=$1","23514",p.f.tenant);err!=nil{return err}
  if err:=reject("SELECT app.orders_transition($1,'paid','generic-compensation',-1)","22023",p.start.OrderID);err!=nil{return err}
  if err:=reject("UPDATE app.orders SET refunded_minor=0,state='paid',version=version+1 WHERE tenant_id=$1 AND id=$2","23514",p.f.tenant,p.start.OrderID);err!=nil{return err}
  // Always roll privileged fixture money back: preparation must have no financial effect.
  return errors.New("C19D fixture rollback")
 })
 if err==nil||err.Error()!="C19D fixture rollback"{t.Fatal(err)}
 if after:=p.witness(t);after!=before{t.Fatalf("preparation changed money: before=%+v after=%+v",before,after)}
}
