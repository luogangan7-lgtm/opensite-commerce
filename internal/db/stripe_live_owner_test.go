//go:build pgx && stripe_live

// Responsibility: owner-dispatched live entry; compiled before the secret step.
// Dependencies: isolated fixture and live adapter; caller: stripe-live-smoke only.
// Raw test output is captured/discarded by stripe_live_ci.py; no live PR execution.
package db_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

func TestStripeLiveOwnerGate(t *testing.T){
	if os.Getenv("GITHUB_EVENT_NAME")!="workflow_dispatch"||os.Getenv("GITHUB_REF")!="refs/heads/main"||os.Getenv("OPENSITE_DISPOSABLE_PG")!="stripe-live"{t.Fatal("owner-dispatch gate required")}
	n:=0;var e error
	if raw:=os.Getenv("STRIPE_REFUND_ATTEMPT");raw!=""{n,e=strconv.Atoi(raw);if e!=nil||n<2{t.Fatal("invalid replacement attempt")}}
	o:=payments.LiveSmokeOptions{Phase:os.Getenv("STRIPE_SMOKE_PHASE"),RunID:os.Getenv("STRIPE_SMOKE_RUN_ID"),Account:os.Getenv("STRIPE_SMOKE_ACCOUNT"),RefundAttempt:n}
	f,s:=liveSmokeFixture(t,o)
	order:=""
	original,e:=s.PrepareRecovery(f.ctx);if e!=nil{t.Fatal("recovery lookup failed; original run remains open")}
	if original!="A"{
		r,e:=f.service.Start(checkoutRequestContext(f.ctx),f.actor,f.input())
		if e!=nil{
			// Setup/send may have completed remotely before a network error.
			// Attempt cleanup even if local checkout setup could not commit.
			v,_:=s.Execute(context.Background());emitLiveSummary(v);t.Fatal("setup failed; reconcile original run")
		}
		order=r.OrderID
	}
	v,e:=s.Execute(f.ctx)
	if e!=nil{emitLiveSummary(v);t.Fatal("live smoke unresolved; use original run ID for R")}
	liveSmokeWitness(t,f,order,original)
	v.Counts=map[string]int{"capture_journals":0,"refund_journals":0,"extra_replay_journals":0}
	if original!="A"{if _,e=s.Execute(f.ctx);e!=nil{t.Fatal("financial replay failed")};liveSmokeWitness(t,f,order,original);v.Counts["capture_journals"]=1;v.Counts["refund_journals"]=1}
	emitLiveSummary(v)
}
