// Responsibility: project authenticated live Events into the existing Payments
// applyFact path, using a fixture-only live Inbox instead of test-pinned tables.
// Dependencies: ephemeral stripe_smoke schema, caller-owned mapped pgx tx, Checkout.
// Caller: StripeLiveSmoke only. No raw event envelopes or PII enter the database.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func (s *StripeLiveSmoke) confirmEvent(ctx context.Context,o liveObject,refund bool)error{
	kind:="payment_intent.succeeded";if refund{kind="refund.updated"}
	after:="";seen:=map[string]bool{}
	for page:=0;page<100;page++{
		var list struct{
			Data []struct{ID string `json:"id"`;Type string `json:"type"`;Account string `json:"account"`;Livemode bool `json:"livemode"`;Data struct{Object liveObject `json:"object"`} `json:"data"`} `json:"data"`
			More bool `json:"has_more"`
		}
		q:=url.Values{"limit":{"100"},"types[]":{kind}};if refund{q.Add("types[]","refund.created")};if after!=""{q.Set("starting_after",after)}
		if s.request(ctx,"GET","events?"+q.Encode(),"",nil,&list)!=nil{return liveFailure()}
		for _,e:=range list.Data{
			if !liveID(e.ID,"evt")||seen[e.ID]{return liveFailure()};seen[e.ID]=true
			v:=e.Data.Object
			if v.ID!=o.ID{continue}
			if !e.Livemode||(e.Account!=""&&e.Account!=s.options.Account)||v.Amount!=o.Amount||v.Currency!=o.Currency||v.Status!="succeeded"||
				(!refund&&(e.Type!="payment_intent.succeeded"||!s.validObject(v,"B",5000)||v.Metadata["osc_invoice"]!=s.invoiceID))||
				(refund&&((e.Type!="refund.updated"&&e.Type!="refund.created")||v.PaymentIntent!=s.intentID)){return liveFailure()}
			return nil
		}
		if !list.More||len(list.Data)==0{return liveFailure()};after=list.Data[len(list.Data)-1].ID
	}
	return liveFailure()
}

func (s *StripeLiveSmoke) ingestLiveFact(ctx context.Context,o liveObject,refund bool)error{
	if s.confirmEvent(ctx,o,refund)!=nil{return liveFailure()}
	kind:="capture";status:=payment.StatusCaptured;ref:=o.ID;refundID:=""
	if refund{kind="refund";status=payment.StatusRefunded;ref=s.intentID;refundID=o.ID}
	identity:=sha256.Sum256([]byte("stripe:live:"+s.options.Account+":"+s.options.RunID+":"+kind+":"+o.ID))
	key:=hex.EncodeToString(identity[:])
	event:=payment.VerifiedEvent{EventID:key,TenantID:s.bound.TenantID,OrderID:s.bound.OrderID,OrderVersion:s.bound.OrderVersion,ProviderRef:ref,Status:status,Amount:money.Money{Amount:o.Amount,Currency:"USD"}}
	if event.Validate()!=nil{return liveFailure()}
	if s.options.Receive==nil{return liveFailure()}
	e:=s.options.Receive(ctx,s.options.Account,event,refundID,func(ctx context.Context,tx pgx.Tx)(bool,error){return (&Module{config:s.config}).applyFact(ctx,tx,event,refundID,s.options.Account)})
	if e!=nil{return liveFailure()};return nil
}
