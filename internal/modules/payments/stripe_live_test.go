// Responsibility: offline live-smoke lifecycle and public-output regressions.
// Dependencies: fake Stripe transport and projected fact consumer; caller: normal CI.
// Invariants: no network, no card data, no duplicate refund or private output.
package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

const liveCanaryURL = "https://invoice.stripe.test/private-canary-payable"
const liveCanaryEmail = "owner-canary@example.test"

type liveFake struct {
	invoice, intent, refund string
	refunds []liveObject
	creates, voids, sends, captureFacts, refundFacts int
	pages, page int
	voidRace, transportFail, failCapture, delayed bool
	corrupt string
	seen map[string]bool
	requests []string
	s *StripeLiveSmoke
}

func newLiveFake(t *testing.T, phase string, attempt int) *liveFake {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("STRIPE_API", "rk_live_offline_fixture")
	t.Setenv("STRIPE_SMOKE_EMAIL", liveCanaryEmail)
	f := &liveFake{invoice:"paid", intent:"succeeded", refund:"succeeded", seen:map[string]bool{}}
	at := time.Date(2026,10,26,0,0,0,0,time.UTC)
	c := Config{Run:func(ctx context.Context, account string, fn func(context.Context,pgx.Tx,string,time.Time) error) error {
		if account != "acct_offline_canary" { return errors.New("wrong mapped account") }
		at = at.Add(31*time.Minute)
		return fn(ctx,nil,"10000000-0000-4000-8000-000000000001",at)
	}, Apply:func(context.Context,pgx.Tx,payment.VerifiedEvent)(bool,error){ return false,errors.New("test must use projected ingestion") }}
	s, err := NewStripeLiveSmoke(c, LiveSmokeOptions{Phase:phase,RunID:"123456",Account:"acct_offline_canary",RefundAttempt:attempt,Receive:func(context.Context,string,payment.VerifiedEvent,string,func(context.Context,pgx.Tx)(bool,error))error{return errors.New("offline ingestion is injected separately")}})
	if err != nil { t.Fatal("offline constructor rejected") }
	f.s = s
	s.client.Transport = stripeTransport(f.roundTrip)
	s.pause = func(context.Context)error{return nil}
	s.ingest = func(_ context.Context, o liveObject, refund bool)error{
		if !refund && f.failCapture { return errors.New("private ingestion failure "+liveCanaryURL+liveCanaryEmail) }
		key := o.ID
		if !f.seen[key] { f.seen[key]=true; if refund { f.refundFacts++ } else { f.captureFacts++ } }
		return nil
	}
	s.bound = payment.CreatePaymentIntent{TenantID:"10000000-0000-4000-8000-000000000001",OrderID:"10000000-0000-4000-8000-000000000002",OrderVersion:1,Amount:money.Money{Amount:5000,Currency:"USD"},IdempotencyKey:"offline"}
	s.invoiceID, s.intentID = "in_full_canary_identifier", "pi_full_canary_identifier"
	return f
}

func (f *liveFake) roundTrip(r *http.Request)(*http.Response,error){
	f.requests=append(f.requests,r.Method+" "+r.URL.Path)
	if r.URL.Host!="api.stripe.com" || r.Header.Get("Stripe-Version")!="2026-09-30.endive" || r.Header.Get("Stripe-Account")!="acct_offline_canary" { return nil,errors.New("transport authority") }
	_ = r.ParseForm()
	meta := map[string]string{"osc_smoke":"123456","osc_phase":"B","osc_invoice":"in_full_canary_identifier"}
	obj := liveObject{ID:"pi_full_canary_identifier",Status:f.intent,Livemode:true,Amount:5000,AmountReceived:5000,Currency:"usd",Metadata:meta,PaymentMethodTypes:[]string{"card"}}
	var out any = obj
	path:=r.URL.Path
	switch {
	case path=="/v1/customers":
		if r.Form.Get("email")!=liveCanaryEmail { return nil,errors.New("email boundary") }; out=map[string]any{"id":"cus_full_canary_identifier","livemode":true,"email":liveCanaryEmail}
	case path=="/v1/invoices" && r.Method=="POST":
		if r.Form.Get("collection_method")!="send_invoice" || r.Form.Get("days_until_due")!="1" || r.Form.Get("payment_settings[payment_method_types][0]")!="card" { return nil,errors.New("invoice configuration") }
		out=liveObject{ID:"in_full_canary_identifier",Livemode:true,Status:"draft",Currency:"usd",Metadata:meta}
	case strings.HasPrefix(path,"/v1/invoices/"):
		if strings.HasSuffix(path,"/void") { f.voids++; if f.voidRace { f.invoice="paid";f.intent="succeeded" } else {f.invoice="void";f.intent="canceled"} }
		if strings.HasSuffix(path,"/send") { f.sends++ }
		methods:=[]string{"card"}; if f.delayed {methods=[]string{"card","us_bank_account"}}
		out=map[string]any{"id":"in_full_canary_identifier","livemode":true,"status":f.invoice,"amount_due":5000,"amount_paid":5000,"currency":"usd","customer":"cus_full_canary_identifier","collection_method":"send_invoice","metadata":meta,"payment_settings":map[string]any{"payment_method_types":methods},"payments":map[string]any{"data":[]any{map[string]any{"payment":map[string]any{"type":"payment_intent","payment_intent":"pi_full_canary_identifier"}}},"has_more":false},"hosted_invoice_url":liveCanaryURL,"invoice_pdf":liveCanaryURL,"customer_email":liveCanaryEmail}
	case path=="/v1/invoices":
		out=map[string]any{"data":[]any{map[string]any{"id":"in_full_canary_identifier","metadata":meta}},"has_more":false}
	case path=="/v1/events":
		ref:=obj
		typ:="payment_intent.succeeded"
		if r.URL.Query().Get("types[]")=="refund.updated" { typ="refund.updated";if len(f.refunds)>0 {ref=f.refunds[len(f.refunds)-1]} }
		out=map[string]any{"data":[]any{map[string]any{"id":"evt_full_canary_identifier","type":typ,"account":"acct_offline_canary","livemode":true,"data":map[string]any{"object":ref},"customer_email":liveCanaryEmail,"url":liveCanaryURL}},"has_more":false}
	case path=="/v1/refunds" && r.Method=="GET":
		if f.pages>0 && r.URL.Query().Get("starting_after")=="" {out=map[string]any{"data":[]liveObject{{ID:"re_page_failed",Status:"failed",Amount:5000,Currency:"usd",PaymentIntent:obj.ID}},"has_more":true};f.page++} else {out=map[string]any{"data":f.refunds,"has_more":false}}
	case path=="/v1/refunds" && r.Method=="POST":
		f.creates++
		if r.Form.Get("payment_intent")!=obj.ID || r.Form.Get("amount")!="5000" || r.Header.Get("Idempotency-Key")=="" {return nil,errors.New("refund request binding")}
		m:=map[string]string{};for k,v:=range r.PostForm {if strings.HasPrefix(k,"metadata["){m[strings.TrimSuffix(strings.TrimPrefix(k,"metadata["),"]")]=v[0]}}
		v:=liveObject{ID:"re_full_canary_identifier",Status:f.refund,Amount:5000,Currency:"usd",PaymentIntent:obj.ID,Metadata:m}
		f.refunds=append(f.refunds,v);out=v
		if f.transportFail {return nil,errors.New("unknown "+liveCanaryURL+liveCanaryEmail)}
	case path=="/v1/payment_intents" && r.Method=="POST":
		obj.ID="pi_phase_a_canary";obj.Amount=50;obj.AmountReceived=0;obj.Status="requires_payment_method";obj.Metadata=map[string]string{"osc_smoke":"123456","osc_phase":"A"};out=obj
	case strings.HasSuffix(path,"/cancel"):
		obj.ID="pi_phase_a_canary";obj.Amount=50;obj.AmountReceived=0;obj.Status="canceled";obj.Metadata=map[string]string{"osc_smoke":"123456","osc_phase":"A"};out=obj
	case strings.HasPrefix(path,"/v1/payment_intents/"):
		if f.intent!="succeeded" {obj.AmountReceived=0};out=obj
	default:return nil,errors.New("unexpected endpoint")
	}
	if v,ok:=out.(liveObject);ok{switch f.corrupt{case "mode":v.Livemode=false;case "source":v.Metadata=map[string]string{"osc_smoke":"999","osc_phase":"B"};case "amount":v.Amount=5001};out=v}
	b,_:=json.Marshal(out)
	return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader(string(b))),Header:make(http.Header)},nil
}

func liveResult(t *testing.T,f *liveFake)(LiveSmokeSummary,error){
	t.Helper();v,e:=f.s.Execute(context.Background()); b,_:=json.Marshal(v)
	all:=string(b);if e!=nil {all+=e.Error()}
	for _,canary:=range []string{liveCanaryURL,liveCanaryEmail,"pi_full_canary_identifier","in_full_canary_identifier","re_full_canary_identifier","acct_offline_canary","rk_live_offline_fixture"}{if strings.Contains(all,canary){t.Fatal("public output leaked canary")}}
	return v,e
}

func TestStripeLiveLifecycleOffline(t *testing.T){
	t.Run("A_create_cancel_zero_facts",func(t *testing.T){f:=newLiveFake(t,"A",0);v,e:=liveResult(t,f);if e!=nil||v.Status!="CANCELED"||f.captureFacts+f.refundFacts!=0{t.Fatal("A acceptance")}})
	t.Run("B_paid_full_refund_replay",func(t *testing.T){f:=newLiveFake(t,"B",0);v,e:=liveResult(t,f);if e!=nil||v.Status!="REFUNDED"||f.creates!=1||f.captureFacts!=1||f.refundFacts!=1{t.Fatal("B acceptance")};_,e=liveResult(t,f);if e!=nil||f.creates!=1||f.captureFacts!=1||f.refundFacts!=1{t.Fatal("replay duplicated financial facts")}})
	t.Run("timeout_open_void",func(t *testing.T){f:=newLiveFake(t,"B",0);f.invoice="open";f.intent="requires_payment_method";v,e:=liveResult(t,f);if e==nil||v.Status!="TIMED_OUT"||f.voids!=1||f.creates!=0{t.Fatal("timeout did not verify unpaid terminal state")}})
	t.Run("payment_between_poll_and_void",func(t *testing.T){f:=newLiveFake(t,"B",0);f.invoice="open";f.intent="requires_payment_method";f.voidRace=true;_,e:=liveResult(t,f);if e==nil||f.creates!=1||f.captureFacts!=1||f.refundFacts!=1{t.Fatal("void race lost payment/refund or accepted timeout")}})
	t.Run("processing_later_R",func(t *testing.T){f:=newLiveFake(t,"B",0);f.invoice="open";f.intent="processing";v,e:=liveResult(t,f);if e==nil||v.Status!="PENDING"||f.voids+f.creates!=0{t.Fatal("processing was closed")};f.s.options.Phase="R";f.invoice="paid";f.intent="succeeded";v,e=liveResult(t,f);if e!=nil||v.Status!="REFUNDED"||f.creates!=1{t.Fatal("R failed recovery")}})
	t.Run("failure_after_payment_still_refunds",func(t *testing.T){f:=newLiveFake(t,"B",0);f.failCapture=true;_,e:=liveResult(t,f);if e==nil||f.creates!=1{t.Fatal("post-payment failure skipped cleanup")}})
	for _,terminal:=range []string{"failed","canceled"}{t.Run("refund_"+terminal,func(t *testing.T){f:=newLiveFake(t,"R",0);f.refunds=[]liveObject{{ID:"re_failed",Status:terminal,Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID,Metadata:map[string]string{"osc_smoke":"123456","osc_refund_attempt":"1"}}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||!v.OwnerActionNeeded||f.creates!=0{t.Fatal("failed refund closed or replaced without consent")};f.s.options.RefundAttempt=2;v,e=liveResult(t,f);if e!=nil||v.Status!="REFUNDED"||f.creates!=1{t.Fatal("authorized replacement")};_,e=liveResult(t,f);if e!=nil||f.creates!=1{t.Fatal("same attempt created duplicate")}})}
	for _,status:=range []string{"succeeded","pending","unknown","requires_action"}{t.Run("existing_"+status,func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_existing",Status:status,Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if f.creates!=0||(status=="succeeded"&&(e!=nil||v.Status!="REFUNDED"))||(status!="succeeded"&&(e==nil||v.Status!="PENDING")){t.Fatal("existing refund replaced or state misclassified")}})}
	t.Run("same_failed_attempt_never_recreated",func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_failed2",Status:"failed",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID,Metadata:map[string]string{"osc_smoke":"123456","osc_refund_attempt":"2"}}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||f.creates!=0{t.Fatal("same failed attempt replaced")}})
	t.Run("full_history_pagination",func(t *testing.T){f:=newLiveFake(t,"R",2);f.pages=1;f.refunds=[]liveObject{{ID:"re_existing",Status:"succeeded",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};_,e:=liveResult(t,f);if e!=nil||f.page==0||f.creates!=0{t.Fatal("refund pagination incomplete")}})
	t.Run("partial_success_never_over_refunded",func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_partial",Status:"succeeded",Amount:4999,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||f.creates!=0{t.Fatal("partial refund closed or over-refunded")}})
	t.Run("runner_kill_and_unknown_network",func(t *testing.T){f:=newLiveFake(t,"B",0);f.transportFail=true;v,e:=liveResult(t,f);if e==nil||v.Status!="PENDING"{t.Fatal("unknown request accepted")};saved:=f.refunds;g:=newLiveFake(t,"R",0);g.refunds=saved;v,e=liveResult(t,g);if e!=nil||v.Status!="REFUNDED"||g.creates!=0||g.captureFacts!=1||g.refundFacts!=1{t.Fatal("new runner failed reconstruction")}})
	t.Run("delayed_method_rejected",func(t *testing.T){f:=newLiveFake(t,"B",0);f.delayed=true;_,e:=liveResult(t,f);if e==nil||f.creates+f.sends!=0{t.Fatal("delayed method admitted")}})
	for _,corrupt:=range []string{"mode","source","amount"}{t.Run("rejected_"+corrupt,func(t *testing.T){f:=newLiveFake(t,"R",0);f.corrupt=corrupt;_,e:=liveResult(t,f);if e==nil||f.creates!=0{t.Fatal("mismatched provider fact admitted")}})}
	t.Run("manual_reimbursement_never_passes",func(t *testing.T){f:=newLiveFake(t,"R",0);f.refunds=[]liveObject{{ID:"re_closed",Status:"failed",FailureReason:"lost_or_stolen_card",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED_MANUAL"||!v.OwnerActionNeeded||f.creates!=0{t.Fatal("manual fallback closed or created money")}})
	t.Run("permission_endpoint_capabilities",func(t *testing.T){for _,path:=range []string{"charges","account","checkout/sessions","payment_methods","invoices/in_x/pay","payment_intents/pi_x/confirm"}{if liveEndpoint("POST",path){t.Fatal("broader provider capability admitted")}}})
}

func TestStripeLiveAdmissionOffline(t *testing.T){
	f:=newLiveFake(t,"A",0)
	for _,key:=range []string{"sk_live_forbidden","sk_test_forbidden","rk_test_forbidden","rk_live_bad\n",""}{t.Setenv("STRIPE_API",key);if _,e:=NewStripeLiveSmoke(f.s.config,f.s.options);e==nil{t.Fatal("unrestricted or test key admitted")}}
	t.Setenv("STRIPE_API","rk_live_offline_fixture")
	for _,attempt:=range []int{-1,1}{o:=f.s.options;o.Phase="R";o.RefundAttempt=attempt;if _,e:=NewStripeLiveSmoke(f.s.config,o);e==nil{t.Fatal("invalid refund attempt")}}
	t.Setenv("GITHUB_REF","refs/heads/feature");if _,e:=NewStripeLiveSmoke(f.s.config,f.s.options);e==nil{t.Fatal("non-main admission")}
}
