//go:build pgx

// Responsibility: compose the CI-only live adapter with real Checkout/Orders/
// Payments/Ledger and a separate live account/Inbox in disposable PostgreSQL.
// Dependencies: existing checkout PG fixture and test-only stripe_smoke DDL.
// Callers: offline fake lifecycle and explicitly owner-dispatched live harness.
// No source email, raw response, live secret or card data is persisted here.
package db_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	ch "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

type liveSmokeOrders struct { oc.Service }
func (o liveSmokeOrders) Create(ctx context.Context,tx pgx.Tx,tenant string,in oc.CreateInput)(oc.Order,error){
	// Harness-only reservation lifetime covers the full 30-minute wait plus
	// reimbursement/replay. Production defaults and invariants are unchanged.
	in.ReservationTTLSeconds=2400
	return o.Service.Create(ctx,tx,tenant,in)
}

func liveSmokeFixture(t *testing.T,o payments.LiveSmokeOptions)(*checkoutPGFixture,*payments.StripeLiveSmoke){
	t.Helper()
	f:=newCheckoutPGFixture(t,100,45) // 4500 net + 450 tax + 50 shipping = 5000.
	ctx,cancel:=context.WithTimeout(context.Background(),36*time.Minute);t.Cleanup(cancel);f.ctx=ctx
	ddl,e:=os.ReadFile("../../tests/fixtures/stripe_live.sql");if e!=nil{t.Fatal("live fixture unavailable")}
	e=f.runner.InMaintenance(ctx,func(ctx context.Context,tx pgx.Tx)error{
		if _,e:=tx.Exec(ctx,string(ddl));e!=nil{return e}
		_,e:=tx.Exec(ctx,"INSERT INTO stripe_smoke.accounts(tenant_id,account,run_id) VALUES($1,$2,$3)",f.tenant,o.Account,o.RunID);return e
	})
	if e!=nil{t.Fatal("isolated live account setup failed")}
	t.Cleanup(func(){ctx,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel();_ = f.runner.InMaintenance(ctx,func(ctx context.Context,tx pgx.Tx)error{_,e:=tx.Exec(ctx,"DROP SCHEMA stripe_smoke CASCADE");return e})})
	var svc ch.Service
	o.Receive=func(ctx context.Context,account string,event payment.VerifiedEvent,refundID string,apply func(context.Context,pgx.Tx)(bool,error))error{
		if account!=o.Account||event.TenantID!=f.tenant{return errors.New("live mapping rejected")}
		projection,_:=json.Marshal(struct{Event payment.VerifiedEvent;RefundID string}{event,refundID});hash:=sha256.Sum256(projection)
		return f.run(ctx,f.tenant,func(ctx context.Context,tx pgx.Tx)error{
			var at time.Time
			if e:=tx.QueryRow(ctx,"SELECT clock_timestamp() FROM stripe_smoke.accounts WHERE tenant_id=$1 AND account=$2 AND run_id=$3 AND environment='live'",f.tenant,account,o.RunID).Scan(&at);e!=nil{return e}
			if _,e:=tx.Exec(ctx,"SELECT set_config('app.payment_provider','stripe',true),pg_advisory_xact_lock(hashtextextended('payments:stripe:live:'||$1||':'||$2,19))",account,event.ProviderRef);e!=nil{return e}
			if _,e:=tx.Exec(ctx,"INSERT INTO stripe_smoke.inbox(tenant_id,account,event_key,payload_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING",f.tenant,account,event.EventID,hash[:]);e!=nil{return e}
			var prior []byte;var applied bool
			if e:=tx.QueryRow(ctx,"SELECT payload_hash,applied FROM stripe_smoke.inbox WHERE tenant_id=$1 AND account=$2 AND event_key=$3 FOR UPDATE",f.tenant,account,event.EventID).Scan(&prior,&applied);e!=nil||!bytes.Equal(prior,hash[:]){return errors.New("live identity conflict")}
			if applied{return nil}
			done,e:=apply(ctx,tx);if e!=nil{return e};if !done{return errors.New("live fact not applied")}
			_,e=tx.Exec(ctx,"UPDATE stripe_smoke.inbox SET applied=true WHERE tenant_id=$1 AND account=$2 AND event_key=$3",f.tenant,account,event.EventID);return e
		})
	}
	adapter,e:=payments.NewStripeLiveSmoke(payments.Config{
		Run:func(ctx context.Context,account string,fn func(context.Context,pgx.Tx,string,time.Time)error)error{
			return f.run(ctx,f.tenant,func(ctx context.Context,tx pgx.Tx)error{
				var at time.Time
				if e:=tx.QueryRow(ctx,"SELECT clock_timestamp() FROM stripe_smoke.accounts WHERE tenant_id=$1 AND account=$2 AND run_id=$3 AND environment='live'",f.tenant,account,o.RunID).Scan(&at);e!=nil{return e}
				if _,e:=tx.Exec(ctx,"SELECT set_config('app.payment_provider','stripe',true)");e!=nil{return e}
				return fn(ctx,tx,f.tenant,at)
			})
		},
		Apply:func(ctx context.Context,tx pgx.Tx,event payment.VerifiedEvent)(bool,error){return svc.(ch.PaymentEventApplier).ApplyPaymentEvent(ctx,tx,event)},
	},o)
	if e!=nil{t.Fatal("live adapter admission failed")}
	rules:=checkout.NewSQLRules(checkout.RuleMetadata{Profiles:map[string]string{f.tenant:f.profile},SKUs:map[string]map[string]checkout.SKUMetadata{f.tenant:{f.sku:{TaxCategory:"standard",WeightG:100}}}})
	svc=checkout.New(checkout.Config{Run:f.run,Cart:f.cart,Rules:rules,Orders:liveSmokeOrders{orders.New()},Ledger:payments.RefundJournal{Ledger:ledger.New()},Payment:adapter})
	f.service=svc
	return f,adapter
}

func liveSmokeWitness(t *testing.T,f *checkoutPGFixture,order string,phase string){
	t.Helper()
	if phase=="A"{
		var n int
		e:=f.run(f.ctx,f.tenant,func(ctx context.Context,tx pgx.Tx)error{return tx.QueryRow(ctx,"SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1",f.tenant).Scan(&n)})
		if e!=nil||n!=0{t.Fatal("A produced financial journals")};return
	}
	w:=f.witness(t,order)
	if w.state!="refunded"||w.journals!=2||w.debits!=w.credits||w.cash!=0||w.receivable!=0{t.Fatal("paid/refunded financial invariant failed")}
	var inbox,applied,testFacts,testAccounts int
	e:=f.runner.InMaintenance(f.ctx,func(ctx context.Context,tx pgx.Tx)error{
		return tx.QueryRow(ctx,`SELECT (SELECT count(*) FROM stripe_smoke.inbox WHERE tenant_id=$1),(SELECT count(*) FROM stripe_smoke.inbox WHERE tenant_id=$1 AND applied),(SELECT count(*) FROM app.payment_events WHERE tenant_id=$1),(SELECT count(*) FROM app.payment_provider_accounts WHERE tenant_id=$1)`,f.tenant).Scan(&inbox,&applied,&testFacts,&testAccounts)
	})
	if e!=nil||inbox!=2||applied!=2||testFacts!=0||testAccounts!=0{t.Fatal("live reconciliation or namespace isolation failed")}
}

func emitLiveSummary(v payments.LiveSmokeSummary){b,_:=json.Marshal(v);fmt.Println("OSC_LIVE_SUMMARY "+string(b))}
