//go:build pgx

// Responsibility: offline fake-Stripe lifecycle with real migrated PostgreSQL.
// Dependencies: actual Payments ingestion, Checkout/Orders/Ledger and ephemeral
// live schema. Caller: normal foundation-pgx CI; never uses real Stripe secrets.
package db_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

type livePGTransport struct { sent bool; creates,refundCreates int; refunded bool; scenario string }
func (f *livePGTransport) RoundTrip(r *http.Request)(*http.Response,error){
	_ = r.ParseForm()
	if r.Body!=nil{defer r.Body.Close()}
	if f.scenario=="failure"{return &http.Response{StatusCode:500,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(`{"error":{"message":"https://invoice.example.test/private-canary owner-canary@example.test pi_full_private_identifier"}}`))},nil}
	meta:=map[string]string{"osc_smoke":"246810","osc_phase":"B","osc_invoice":"in_offlinepg","osc_customer":"cus_offlinepg"}
	pi:=map[string]any{"id":"pi_offlinepg","customer":"cus_offlinepg","status":"requires_payment_method","livemode":true,"amount":5000,"amount_received":0,"currency":"usd","metadata":meta,"payment_method_types":[]string{"card"}}
	if f.sent{pi["status"]="succeeded";pi["amount_received"]=5000}
	refund:=map[string]any{"id":"re_offlinepg","status":"succeeded","amount":5000,"currency":"usd","payment_intent":"pi_offlinepg","metadata":map[string]string{"osc_smoke":"246810","osc_refund_attempt":"1","osc_invoice":"in_offlinepg","osc_phase":"B"}}
	inv:=map[string]any{"id":"in_offlinepg","customer":"cus_offlinepg","status":"open","livemode":true,"amount_due":5000,"amount_paid":0,"currency":"usd","collection_method":"send_invoice","metadata":meta,"payment_settings":map[string]any{"payment_method_types":[]string{"card"}},"payments":map[string]any{"data":[]any{map[string]any{"payment":map[string]any{"type":"payment_intent","payment_intent":"pi_offlinepg"}}},"has_more":false},"hosted_invoice_url":"https://invoice.example.test/private-canary","customer_email":"private-canary@example.test"}
	var out any=pi
	switch{
	case r.URL.Path=="/v1/customers":out=map[string]any{"id":"cus_offlinepg","livemode":true}
	case r.URL.Path=="/v1/invoices"&&r.Method=="POST":f.creates++;out=inv
	case r.URL.Path=="/v1/invoices":out=map[string]any{"data":[]any{inv},"has_more":false}
	case strings.HasPrefix(r.URL.Path,"/v1/invoices/"):
		if strings.HasSuffix(r.URL.Path,"/send"){f.sent=true};if f.sent{inv["status"]="paid";inv["amount_paid"]=5000};out=inv
	case r.URL.Path=="/v1/events":
		typ:="payment_intent.succeeded";var object any=pi;if r.URL.Query().Get("types[]")=="refund.updated"{typ="refund.updated";object=refund}
		out=map[string]any{"data":[]any{map[string]any{"id":"evt_offlinepg","type":typ,"account":"acct_offlinepg","livemode":true,"data":map[string]any{"object":object},"email":"private-canary@example.test"}},"has_more":false}
	case r.URL.Path=="/v1/refunds"&&r.Method=="POST":f.refunded=true;f.refundCreates++;out=refund
	case r.URL.Path=="/v1/refunds":data:=[]any{};if f.refunded{data=append(data,refund)};out=map[string]any{"data":data,"has_more":false}
	}
	b,_:=json.Marshal(out);return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(string(b)))},nil
}

func TestStripeLiveOfflinePGReconstruction(t *testing.T){
	t.Setenv("GITHUB_ACTIONS","true");t.Setenv("GITHUB_EVENT_NAME","workflow_dispatch");t.Setenv("GITHUB_REF","refs/heads/main")
	t.Setenv("STRIPE_API","rk_live_OFFLINE_NOT_A_REAL_CREDENTIAL");t.Setenv("STRIPE_SMOKE_EMAIL","offline@example.test")
	fake:=&livePGTransport{}
	old:=http.DefaultTransport;http.DefaultTransport=fake;t.Cleanup(func(){http.DefaultTransport=old})
	for _,phase:=range []string{"B","R"}{t.Run(phase,func(t *testing.T){
		f,s:=liveSmokeFixture(t,payments.LiveSmokeOptions{Phase:phase,RunID:"246810",Account:"acct_offlinepg"})
		r:=f.start(t)
		v,e:=s.Execute(f.ctx);if e!=nil||v.Status!="REFUNDED"{t.Fatal("real PG fake-provider lifecycle failed")}
		liveSmokeWitness(t,f,r.OrderID,phase)
		before:=f.witness(t,r.OrderID)
		if _,e:=s.Execute(f.ctx);e!=nil||f.witness(t,r.OrderID)!=before{t.Fatal("financial replay was not inert")}
		liveSmokeWitness(t,f,r.OrderID,phase)
	})}
	if fake.creates!=1||fake.refundCreates!=1{t.Fatal("reconstructed runner repeated provider writes")}
}
