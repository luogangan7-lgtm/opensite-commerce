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
	tick time.Duration
	aOnly bool
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
	f := &liveFake{invoice:"paid", intent:"succeeded", refund:"succeeded", seen:map[string]bool{},tick:31*time.Minute}
	at := time.Date(2026,10,26,0,0,0,0,time.UTC)
	c := Config{Run:func(ctx context.Context, account string, fn func(context.Context,pgx.Tx,string,time.Time) error) error {
		if account != "acct_offline_canary" { return errors.New("wrong mapped account") }
		at = at.Add(f.tick)
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
	meta := map[string]string{"osc_smoke":"123456","osc_phase":"B","osc_invoice":"in_full_canary_identifier","osc_customer":"cus_full_canary_identifier"}
	obj := liveObject{ID:"pi_full_canary_identifier",Customer:"cus_full_canary_identifier",Status:f.intent,Livemode:true,Amount:5000,AmountReceived:5000,Currency:"usd",Metadata:meta,PaymentMethodTypes:[]string{"card"}}
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
		data:=[]any{map[string]any{"id":"in_full_canary_identifier","metadata":meta}};if f.aOnly{data=[]any{}};out=map[string]any{"data":data,"has_more":false}
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
	case path=="/v1/payment_intents" && r.Method=="GET":
		out=map[string]any{"data":[]liveObject{{ID:"pi_phase_a_canary",Livemode:true,Amount:50,Currency:"usd",Status:"canceled",Metadata:map[string]string{"osc_smoke":"123456","osc_phase":"A"}}},"has_more":false}
	case path=="/v1/payment_intents/pi_phase_a_canary" && r.Method=="GET":
		out=liveObject{ID:"pi_phase_a_canary",Livemode:true,Amount:50,Currency:"usd",Status:"canceled",Metadata:map[string]string{"osc_smoke":"123456","osc_phase":"A"}}
	case strings.HasPrefix(path,"/v1/payment_intents/"):
		if f.intent!="succeeded" {obj.AmountReceived=0};out=obj
	default:return nil,errors.New("unexpected endpoint")
	}
	if v,ok:=out.(liveObject);ok{switch f.corrupt{case "mode":v.Livemode=false;case "source":v.Metadata=map[string]string{"osc_smoke":"999","osc_phase":"B"};case "amount":v.Amount=5001;case "customer":v.Customer="cus_wrong"};out=v}
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
	t.Run("R_verified_payment_canceled",func(t *testing.T){f:=newLiveFake(t,"R",0);f.invoice="void";f.intent="canceled";v,e:=liveResult(t,f);if e==nil||v.Status!="CANCELED"||f.captureFacts+f.refundFacts+f.creates!=0{t.Fatal("terminal payment state was not explicit or manufactured money")}})
	t.Run("R_A_already_canceled_no_repeat_command",func(t *testing.T){f:=newLiveFake(t,"R",0);f.aOnly=true;phase,e:=f.s.PrepareRecovery(context.Background());if e!=nil||phase!="A"{t.Fatal("A recovery lookup")};v,e:=liveResult(t,f);if e!=nil||v.Status!="CANCELED"||v.AmountMinor!=50||f.captureFacts+f.refundFacts!=0{t.Fatal("A recovery acceptance")};for _,r:=range f.requests{if strings.HasPrefix(r,"POST "){t.Fatal("A recovery repeated provider write")}}})
	t.Run("failure_after_payment_still_refunds",func(t *testing.T){f:=newLiveFake(t,"B",0);f.failCapture=true;_,e:=liveResult(t,f);if e==nil||f.creates!=1{t.Fatal("post-payment failure skipped cleanup")}})
	t.Run("new_failed_refund_remains_unresolved",func(t *testing.T){f:=newLiveFake(t,"B",0);f.refund="failed";v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||!v.OwnerActionNeeded||f.refundFacts!=0{t.Fatal("new failed refund closed or posted")}})
	for _,field:=range []string{"amount","run","invoice","attempt","phase"}{t.Run("polled_binding_"+field,func(t *testing.T){f:=newLiveFake(t,"B",0);f.refund="pending";f.tick=time.Second;f.s.pause=func(context.Context)error{r:=f.refunds[0];r.Status="succeeded";switch field{case "amount":r.Amount=4999;case "run":r.Metadata["osc_smoke"]="999";case "invoice":r.Metadata["osc_invoice"]="in_other";case "attempt":r.Metadata["osc_refund_attempt"]="2";case "phase":r.Metadata["osc_phase"]="A"};f.refunds[0]=r;return nil};v,e:=liveResult(t,f);if e==nil||v.Status!="PENDING"||f.refundFacts!=0{t.Fatal("polled refund lost original binding")}})}
	for _,terminal:=range []string{"failed","canceled"}{t.Run("refund_"+terminal,func(t *testing.T){f:=newLiveFake(t,"R",0);f.refunds=[]liveObject{{ID:"re_failed",Status:terminal,Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID,Metadata:map[string]string{"osc_smoke":"123456","osc_refund_attempt":"1"}}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||!v.OwnerActionNeeded||f.creates!=0{t.Fatal("failed refund closed or replaced without consent")};f.s.options.RefundAttempt=2;v,e=liveResult(t,f);if e!=nil||v.Status!="REFUNDED"||f.creates!=1{t.Fatal("authorized replacement")};_,e=liveResult(t,f);if e!=nil||f.creates!=1{t.Fatal("same attempt created duplicate")}})}
	for _,status:=range []string{"succeeded","pending","unknown","requires_action"}{t.Run("existing_"+status,func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_existing",Status:status,Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if f.creates!=0||(status=="succeeded"&&(e!=nil||v.Status!="REFUNDED"))||(status!="succeeded"&&(e==nil||v.Status!="PENDING")){t.Fatal("existing refund replaced or state misclassified")}})}
	t.Run("same_failed_attempt_never_recreated",func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_failed2",Status:"failed",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID,Metadata:map[string]string{"osc_smoke":"123456","osc_refund_attempt":"2"}}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||f.creates!=0{t.Fatal("same failed attempt replaced")}})
	t.Run("unidentified_failed_attempt_never_recreated",func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_unidentified",Status:"failed",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||f.creates!=0{t.Fatal("missing durable identity authorized a replacement")}})
	t.Run("full_history_pagination",func(t *testing.T){f:=newLiveFake(t,"R",2);f.pages=1;f.refunds=[]liveObject{{ID:"re_existing",Status:"succeeded",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};_,e:=liveResult(t,f);if e!=nil||f.page==0||f.creates!=0{t.Fatal("refund pagination incomplete")}})
	t.Run("partial_success_never_over_refunded",func(t *testing.T){f:=newLiveFake(t,"R",2);f.refunds=[]liveObject{{ID:"re_partial",Status:"succeeded",Amount:4999,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED"||f.creates!=0{t.Fatal("partial refund closed or over-refunded")}})
	t.Run("runner_kill_and_unknown_network",func(t *testing.T){f:=newLiveFake(t,"B",0);f.transportFail=true;v,e:=liveResult(t,f);if e==nil||v.Status!="PENDING"{t.Fatal("unknown request accepted")};saved:=f.refunds;g:=newLiveFake(t,"R",0);g.refunds=saved;v,e=liveResult(t,g);if e!=nil||v.Status!="REFUNDED"||g.creates!=0||g.captureFacts!=1||g.refundFacts!=1{t.Fatal("new runner failed reconstruction")}})
	t.Run("delayed_method_rejected",func(t *testing.T){f:=newLiveFake(t,"B",0);f.delayed=true;_,e:=liveResult(t,f);if e==nil||f.creates+f.sends!=0{t.Fatal("delayed method admitted")}})
	t.Run("customer_swap_rejected_before_send",func(t *testing.T){f:=newLiveFake(t,"B",0);f.corrupt="customer";if _,e:=f.s.CreateIntent(context.Background(),f.s.bound);e==nil||f.sends!=0{t.Fatal("customer swap reached private-channel delivery")}})
	for _,corrupt:=range []string{"mode","source","amount","customer"}{t.Run("rejected_"+corrupt,func(t *testing.T){f:=newLiveFake(t,"R",0);f.corrupt=corrupt;_,e:=liveResult(t,f);if e==nil||f.creates!=0{t.Fatal("mismatched provider fact admitted")}})}
	t.Run("manual_reimbursement_never_passes",func(t *testing.T){f:=newLiveFake(t,"R",0);f.refunds=[]liveObject{{ID:"re_closed",Status:"failed",FailureReason:"lost_or_stolen_card",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID}};v,e:=liveResult(t,f);if e==nil||v.Status!="REFUND_UNRESOLVED_MANUAL"||!v.OwnerActionNeeded||f.creates!=0{t.Fatal("manual fallback closed or created money")}})
	t.Run("permission_endpoint_capabilities",func(t *testing.T){for _,path:=range []string{"charges","account","checkout/sessions","payment_methods","invoices/in_x/pay","payment_intents/pi_x/confirm"}{if liveEndpoint("POST",path){t.Fatal("broader provider capability admitted")}}})
	t.Run("mapped_account_rejected_before_network",func(t *testing.T){f:=newLiveFake(t,"R",0);f.s.options.Account="acct_other";_,e:=liveResult(t,f);if e==nil||len(f.requests)!=0{t.Fatal("wrong account reached provider")}})
	t.Run("polled_event_account_rejected",func(t *testing.T){f:=newLiveFake(t,"R",0);_,pi,e:=f.s.state(context.Background());if e!=nil{t.Fatal("fake setup")};f.s.client.Transport=stripeTransport(func(r *http.Request)(*http.Response,error){res,e:=f.roundTrip(r);if e!=nil{return nil,e};b,_:=io.ReadAll(res.Body);res.Body.Close();b=[]byte(strings.ReplaceAll(string(b),"acct_offline_canary","acct_other"));res.Body=io.NopCloser(strings.NewReader(string(b)));return res,nil});if f.s.confirmEvent(context.Background(),pi,false)==nil{t.Fatal("cross-account event admitted")}})
	t.Run("refund_event_metadata_admission",func(t *testing.T){f:=newLiveFake(t,"R",0);r:=liveObject{ID:"re_metadata_canary",Status:"succeeded",Amount:5000,Currency:"usd",PaymentIntent:f.s.intentID,Metadata:map[string]string{"osc_smoke":"123456","osc_phase":"B","osc_invoice":f.s.invoiceID,"osc_refund_attempt":"1"}};f.refunds=[]liveObject{r};if f.s.confirmEvent(context.Background(),r,true)!=nil{t.Fatal("valid refund without nonexistent livemode field rejected")};for _,field:=range []string{"osc_smoke","osc_phase","osc_invoice","osc_refund_attempt"}{bad:=r;bad.Metadata=map[string]string{};for k,v:=range r.Metadata{bad.Metadata[k]=v};bad.Metadata[field]="wrong";f.refunds=[]liveObject{bad};if f.s.confirmEvent(context.Background(),r,true)==nil{t.Fatal("unbound refund event admitted")}}})
}

func TestStripeLiveAdmissionOffline(t *testing.T){
	f:=newLiveFake(t,"A",0)
	for _,key:=range []string{"sk_live_forbidden","sk_test_forbidden","rk_test_forbidden","rk_live_bad\n",""}{t.Setenv("STRIPE_API",key);if _,e:=NewStripeLiveSmoke(f.s.config,f.s.options);e==nil{t.Fatal("unrestricted or test key admitted")}}
	t.Setenv("STRIPE_API","rk_live_offline_fixture")
	for _,attempt:=range []int{-1,1}{o:=f.s.options;o.Phase="R";o.RefundAttempt=attempt;if _,e:=NewStripeLiveSmoke(f.s.config,o);e==nil{t.Fatal("invalid refund attempt")}}
	t.Setenv("GITHUB_REF","refs/heads/feature");if _,e:=NewStripeLiveSmoke(f.s.config,f.s.options);e==nil{t.Fatal("non-main admission")}
}

// The fake deliberately does not retain idempotency keys. Durable provider
// metadata must stop a fresh adapter from creating a second payable identity.
func TestStripeLiveExpiredCreateKeysOffline(t *testing.T) {
	for _,phase:=range []string{"A","B"}{t.Run(phase,func(t *testing.T){
		f:=newLiveFake(t,phase,0)
		t.Setenv("GITHUB_RUN_ATTEMPT","1")
		exists:=false;creates:=0;posts:=0
		collection:="/v1/invoices";prefix:="in";if phase=="A"{collection="/v1/payment_intents";prefix="pi"}
		transport:=stripeTransport(func(r *http.Request)(*http.Response,error){
			if r.Method=="GET"&&(r.URL.Path=="/v1/invoices"||r.URL.Path=="/v1/payment_intents"||r.URL.Path=="/v1/customers"){
				data:=[]any{};if exists&&r.URL.Path==collection{data=append(data,map[string]any{"id":prefix+"_durable","metadata":map[string]string{"osc_smoke":"123456","osc_phase":phase}})}
				body,_:=json.Marshal(map[string]any{"data":data,"has_more":false})
				return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(string(body)))},nil
			}
			if r.Method=="POST"{posts++;if r.URL.Path==collection{creates++;exists=true}}
			return f.roundTrip(r)
		})
		f.s.client.Transport=transport
		run:=func()error{if phase=="A"{_,e:=f.s.Execute(context.Background());return e};_,e:=f.s.CreateIntent(context.Background(),f.s.bound);return e}
		if run()!=nil||creates!=1{t.Fatal("first create failed")}
		before:=posts
		// Discard all adapter memory, preserving only fake provider objects.
		g:=newLiveFake(t,phase,0);g.s.client.Transport=transport;f.s=g.s
		if run()==nil||creates!=1||posts!=before{t.Fatal("expired idempotency key created another smoke object instead of reporting recovery")}
	})}
}

func TestStripeLiveRerunRejectedBeforeNetworkOffline(t *testing.T) {
	for _,phase:=range []string{"A","B"}{for _,attempt:=range []string{"2","99","","0","invalid"}{t.Run(phase+"_"+attempt,func(t *testing.T){
		f:=newLiveFake(t,phase,0);t.Setenv("GITHUB_RUN_ATTEMPT",attempt)
		_,e:=NewStripeLiveSmoke(f.s.config,f.s.options)
		if e==nil||len(f.requests)!=0{t.Fatal("A/B rerun admitted before provider access")}
	})}}
}

func TestStripeLiveCreationLookupFailsClosedOffline(t *testing.T) {
	for _,phase:=range []string{"A","B"}{for _,collection:=range []string{"invoices","payment_intents","customers"}{for _,scenario:=range []string{"existing_second_page","unknown_history","duplicate_cursor"}{t.Run(phase+"_"+collection+"_"+scenario,func(t *testing.T){
		f:=newLiveFake(t,phase,0);t.Setenv("GITHUB_RUN_ATTEMPT","1");posts:=0;pages:=0
		prefix:=map[string]string{"invoices":"in","payment_intents":"pi","customers":"cus"}[collection]
		f.s.client.Transport=stripeTransport(func(r *http.Request)(*http.Response,error){
			if r.Method=="POST"{posts++;return f.roundTrip(r)}
			data:=[]any{};more:=false
			if r.URL.Path=="/v1/"+collection{
				pages++
				if scenario=="unknown_history"{return nil,errors.New("provider history unavailable")}
				metadata:=map[string]string{"osc_smoke":"different"}
				if pages>1&&scenario=="existing_second_page"{metadata["osc_smoke"]="123456"}
				data=append(data,map[string]any{"id":prefix+"_page"+strings.Repeat("x",pages),"metadata":metadata})
				more=pages==1||scenario=="duplicate_cursor"
				if scenario=="duplicate_cursor"{data=[]any{map[string]any{"id":prefix+"_repeat","metadata":metadata}}}
			}
			body,_:=json.Marshal(map[string]any{"data":data,"has_more":more})
			return &http.Response{StatusCode:200,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(string(body)))},nil
		})
		var e error;if phase=="A"{_,e=f.s.Execute(context.Background())}else{_,e=f.s.CreateIntent(context.Background(),f.s.bound)}
		if e==nil||posts!=0||pages==0{t.Fatal("creation bypassed complete durable provider lookup")}
	})}}}
}
