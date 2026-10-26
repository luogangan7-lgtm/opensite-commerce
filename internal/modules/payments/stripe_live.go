// Responsibility: isolated owner-gated Stripe live-smoke adapter and recovery.
// Dependencies: authenticated Stripe HTTP, mapped DB clock, Payments fact ingestion.
// Caller: the CI-only live fixture; production bootstrap never constructs this type.
// Invariants: restricted key, fixed endpoints, card-only, no public provider data,
// no automatic replacement attempts, and no provider IO while a DB lock is held.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

// LiveSmokeOptions contains no credentials. Account is an owner-configured
// Environment variable; Stripe scopes every request to that exact account.
type LiveSmokeOptions struct {
	Phase string
	RunID string
	Account string
	RefundAttempt int
	// Receive owns only the ephemeral live Inbox transaction. The supplied
	// callback is the same Payments applyFact path used by signed ingestion.
	Receive LiveSmokeReceiver
}

type LiveSmokeReceiver func(context.Context,string,payment.VerifiedEvent,string,func(context.Context,pgx.Tx)(bool,error))error

type LiveSmokeSummary struct {
	Phase string `json:"phase"`
	Status string `json:"status"`
	AmountMinor int64 `json:"amount_minor"`
	Currency string `json:"currency"`
	Livemode bool `json:"livemode"`
	OwnerActionNeeded bool `json:"owner_action_needed"`
	Counts map[string]int `json:"counts,omitempty"`
}

// Project only these facts. URLs, PDFs, email, customer details and error
// envelopes have no destination in this decoder or in the persisted projection.
type liveObject struct {
	ID string `json:"id"`
	Status string `json:"status"`
	Livemode bool `json:"livemode"`
	Amount int64 `json:"amount"`
	AmountReceived int64 `json:"amount_received"`
	AmountDue int64 `json:"amount_due"`
	AmountPaid int64 `json:"amount_paid"`
	Currency string `json:"currency"`
	PaymentIntent string `json:"payment_intent"`
	FailureReason string `json:"failure_reason"`
	CollectionMethod string `json:"collection_method"`
	Metadata map[string]string `json:"metadata"`
	PaymentMethodTypes []string `json:"payment_method_types"`
	PaymentSettings struct { PaymentMethodTypes []string `json:"payment_method_types"` } `json:"payment_settings"`
	Payments struct {
		Data []struct { Payment struct { Type string `json:"type"`; PaymentIntent string `json:"payment_intent"` } `json:"payment"` } `json:"data"`
		More bool `json:"has_more"`
	} `json:"payments"`
}

type StripeLiveSmoke struct {
	config Config
	options LiveSmokeOptions
	secret string
	client *http.Client
	bound payment.CreatePaymentIntent
	invoiceID, intentID string
	originalA bool
	pause func(context.Context)error
	ingest func(context.Context,liveObject,bool)error
}

func liveFailure() error { return errors.New("live smoke requires reconciliation; provider details withheld") }
func liveID(s, prefix string) bool { return strings.HasPrefix(s,prefix+"_") && len(s)<=128 && accountPattern.MatchString(s) }
func liveRun(s string) bool { if len(s)==0||len(s)>32{return false};for _,c:=range s{if c<'0'||c>'9'{return false}};return true }

func NewStripeLiveSmoke(c Config, o LiveSmokeOptions)(*StripeLiveSmoke,error){
	key:=os.Getenv("STRIPE_API")
	if os.Getenv("GITHUB_ACTIONS")!="true" || os.Getenv("GITHUB_EVENT_NAME")!="workflow_dispatch" || os.Getenv("GITHUB_REF")!="refs/heads/main" ||
		!strings.HasPrefix(key,"rk_live_") || len(key)<=8 || strings.ContainsFunc(key,func(r rune)bool{return r<'!'||r>'~'}) ||
		!liveID(o.Account,"acct") || !liveRun(o.RunID) || c.Run==nil || c.Apply==nil || o.Receive==nil ||
		(o.Phase!="A"&&o.Phase!="B"&&o.Phase!="R") || o.RefundAttempt<0 || o.RefundAttempt==1 || (o.RefundAttempt!=0&&o.Phase!="R") {
		return nil,liveFailure()
	}
	if o.Phase=="B" { email:=os.Getenv("STRIPE_SMOKE_EMAIL");a,e:=mail.ParseAddress(email);if e!=nil||a.Address!=email||strings.ContainsAny(email,"\r\n"){return nil,liveFailure()} }
	c.Provider="stripe"
	s:=&StripeLiveSmoke{config:c,options:o,secret:key,client:&http.Client{Timeout:15*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}}}
	s.pause=func(ctx context.Context)error{timer:=time.NewTimer(5*time.Second);defer timer.Stop();select{case<-ctx.Done():return liveFailure();case<-timer.C:return nil}}
	s.ingest=s.ingestLiveFact
	return s,nil
}

func (s *StripeLiveSmoke) now(ctx context.Context)(time.Time,error){
	var at time.Time
	e:=s.config.Run(ctx,s.options.Account,func(_ context.Context,_ pgx.Tx,_ string,t time.Time)error{at=t;return nil})
	if e!=nil||at.IsZero(){return at,liveFailure()};return at,nil
}

func (s *StripeLiveSmoke) key(op string) string { h:=sha256.Sum256([]byte("stripe-live:"+s.options.RunID+":"+op));return hex.EncodeToString(h[:]) }
func (s *StripeLiveSmoke) metadata(phase string) url.Values {return url.Values{"metadata[osc_smoke]":{s.options.RunID},"metadata[osc_phase]":{phase}}}

// A capability allowlist is additional defense; it does not certify that a
// Dashboard key has no additional grants. The owner must verify that separately.
func liveEndpoint(method,path string) bool {
	p:=strings.Split(path,"?")[0];parts:=strings.Split(p,"/")
	if len(parts)==1 {
		return (p=="events"&&method=="GET") || (p=="customers"&&method=="POST") || ((p=="invoices"||p=="refunds"||p=="payment_intents")&&(method=="GET"||method=="POST"))
	}
	if len(parts)<2||len(parts)>3{return false}
	kinds:=map[string]string{"invoices":"in","payment_intents":"pi","refunds":"re"}
	if !liveID(parts[1],kinds[parts[0]])||kinds[parts[0]]==""{return false}
	if len(parts)==2{return method=="GET"||(method=="POST"&&parts[0]=="payment_intents")}
	return method=="POST"&&((parts[0]=="payment_intents"&&parts[2]=="cancel")||(parts[0]=="invoices"&&(parts[2]=="add_lines"||parts[2]=="finalize"||parts[2]=="send"||parts[2]=="void")))
}

func (s *StripeLiveSmoke) request(ctx context.Context,method,path,key string,form url.Values,out any)error{
	if !liveEndpoint(method,path){return liveFailure()}
	req,e:=http.NewRequestWithContext(ctx,method,"https://api.stripe.com/v1/"+path,strings.NewReader(form.Encode()));if e!=nil{return liveFailure()}
	req.Header.Set("Authorization","Bearer "+s.secret)
	req.Header.Set("Stripe-Version","2026-09-30.endive")
	req.Header.Set("Stripe-Account",s.options.Account)
	if method=="POST"{if key==""{return liveFailure()};req.Header.Set("Idempotency-Key",key);req.Header.Set("Content-Type","application/x-www-form-urlencoded")}
	res,e:=s.client.Do(req);if e!=nil{return liveFailure()};defer res.Body.Close()
	b,e:=io.ReadAll(io.LimitReader(res.Body,(1<<20)+1));if e!=nil||len(b)>1<<20||res.StatusCode<200||res.StatusCode>=300||json.Unmarshal(b,out)!=nil{return liveFailure()}
	return nil
}

func (s *StripeLiveSmoke) validObject(o liveObject,phase string,amount int64)bool{
	return o.Livemode&&o.Metadata["osc_smoke"]==s.options.RunID&&o.Metadata["osc_phase"]==phase&&o.Currency=="usd"&&o.Amount==amount
}

// No live capture command exists: payment confirmation is exclusively the
// owner's card action in Stripe's private invoice page.
func (s *StripeLiveSmoke) Capture(context.Context,payment.PaymentOperation)(payment.PaymentIntentResult,error){return payment.PaymentIntentResult{},liveFailure()}
func (s *StripeLiveSmoke) Cancel(context.Context,payment.PaymentOperation)(payment.PaymentIntentResult,error){return payment.PaymentIntentResult{},liveFailure()}

func (s *StripeLiveSmoke) CreateIntent(ctx context.Context,r payment.CreatePaymentIntent)(payment.PaymentIntentResult,error){
	if r.Validate()!=nil||r.Amount.Amount!=5000||r.Amount.Currency!="USD"||(s.options.Phase!="B"&&s.options.Phase!="R"){return payment.PaymentIntentResult{},liveFailure()}
	if _,e:=s.now(ctx);e!=nil{return payment.PaymentIntentResult{},e}
	s.bound=r
	if s.options.Phase=="R" {if e:=s.locate(ctx);e!=nil{return payment.PaymentIntentResult{},e}} else {
		var customer liveObject
		f:=s.metadata("B");f.Set("email",os.Getenv("STRIPE_SMOKE_EMAIL"))
		if s.request(ctx,"POST","customers",s.key("customer"),f,&customer)!=nil||!customer.Livemode||!liveID(customer.ID,"cus"){return payment.PaymentIntentResult{},liveFailure()}
		f=s.metadata("B");f.Set("customer",customer.ID);f.Set("collection_method","send_invoice");f.Set("days_until_due","1");f.Set("currency","usd");f.Set("auto_advance","false");f.Set("payment_settings[payment_method_types][0]","card")
		var inv liveObject
		if s.request(ctx,"POST","invoices",s.key("invoice"),f,&inv)!=nil||!liveID(inv.ID,"in")||!inv.Livemode{return payment.PaymentIntentResult{},liveFailure()}
		s.invoiceID=inv.ID
		if s.request(ctx,"POST","invoices/"+inv.ID+"/add_lines",s.key("line"),url.Values{"lines[0][amount]":{"5000"},"lines[0][description]":{"Owner-approved CI smoke"}},&inv)!=nil{return payment.PaymentIntentResult{},liveFailure()}
		if s.request(ctx,"POST","invoices/"+s.invoiceID+"/finalize",s.key("finalize"),url.Values{"auto_advance":{"false"}},&inv)!=nil{return payment.PaymentIntentResult{},liveFailure()}
		if e:=s.readAssociation(ctx);e!=nil{return payment.PaymentIntentResult{},e}
		f=s.metadata("B");f.Set("metadata[osc_invoice]",s.invoiceID)
		var pi liveObject
		if s.request(ctx,"POST","payment_intents/"+s.intentID,s.key("bind"),f,&pi)!=nil||!s.validObject(pi,"B",5000)||pi.Metadata["osc_invoice"]!=s.invoiceID{return payment.PaymentIntentResult{},liveFailure()}
		// Metadata and card-only configuration must be verified before sending.
		if _,_,e:=s.state(ctx);e!=nil{return payment.PaymentIntentResult{},e}
		if s.request(ctx,"POST","invoices/"+s.invoiceID+"/send",s.key("send"),nil,&inv)!=nil{return payment.PaymentIntentResult{},liveFailure()}
	}
	return payment.PaymentIntentResult{ProviderRef:s.intentID,Status:payment.StatusPending},nil
}

func (s *StripeLiveSmoke) locate(ctx context.Context)error{
	ids:=[]string{};after:="";seen:=map[string]bool{}
	for page:=0;page<100;page++{
		var list struct{Data []liveObject `json:"data"`;More bool `json:"has_more"`}
		q:=url.Values{"limit":{"100"}};if after!=""{q.Set("starting_after",after)}
		if s.request(ctx,"GET","invoices?"+q.Encode(),"",nil,&list)!=nil{return liveFailure()}
		for _,o:=range list.Data{if !liveID(o.ID,"in")||seen[o.ID]{return liveFailure()};seen[o.ID]=true;if o.Metadata["osc_smoke"]==s.options.RunID {ids=append(ids,o.ID)}}
		if !list.More{if len(ids)==0{return s.locateA(ctx)};if len(ids)!=1{return liveFailure()};s.invoiceID=ids[0];return s.readAssociation(ctx)}
		if len(list.Data)==0{return liveFailure()};after=list.Data[len(list.Data)-1].ID
	}
	return liveFailure()
}

// PrepareRecovery discovers the original phase without creating provider
// objects. An A run has no Invoice and must not be reconstructed as a B order.
func (s *StripeLiveSmoke) PrepareRecovery(ctx context.Context)(string,error){
	if s.options.Phase!="R"{return s.options.Phase,nil}
	if _,e:=s.now(ctx);e!=nil{return "",e}
	if e:=s.locate(ctx);e!=nil{return "",e};if s.originalA{return "A",nil};return "B",nil
}
func (s *StripeLiveSmoke) locateA(ctx context.Context)error{
	after:="";ids:=[]string{};seen:=map[string]bool{}
	for page:=0;page<100;page++{
		var list struct{Data []liveObject `json:"data"`;More bool `json:"has_more"`}
		q:=url.Values{"limit":{"100"}};if after!=""{q.Set("starting_after",after)}
		if s.request(ctx,"GET","payment_intents?"+q.Encode(),"",nil,&list)!=nil{return liveFailure()}
		for _,o:=range list.Data{if !liveID(o.ID,"pi")||seen[o.ID]{return liveFailure()};seen[o.ID]=true;if o.Metadata["osc_smoke"]==s.options.RunID{if !s.validObject(o,"A",50){return liveFailure()};ids=append(ids,o.ID)}}
		if !list.More{if len(ids)!=1{return liveFailure()};s.intentID=ids[0];s.originalA=true;return nil};if len(list.Data)==0{return liveFailure()};after=list.Data[len(list.Data)-1].ID
	}
	return liveFailure()
}

func (s *StripeLiveSmoke) invoice(ctx context.Context)(liveObject,error){
	var inv liveObject
	e:=s.request(ctx,"GET","invoices/"+s.invoiceID+"?expand[]=payments", "",nil,&inv)
	if e!=nil||inv.ID!=s.invoiceID||!inv.Livemode||inv.Metadata["osc_smoke"]!=s.options.RunID||inv.Metadata["osc_phase"]!="B"||inv.Currency!="usd"||inv.AmountDue!=5000||inv.CollectionMethod!="send_invoice"||len(inv.PaymentSettings.PaymentMethodTypes)!=1||inv.PaymentSettings.PaymentMethodTypes[0]!="card"||inv.Payments.More||len(inv.Payments.Data)!=1||inv.Payments.Data[0].Payment.Type!="payment_intent"||!liveID(inv.Payments.Data[0].Payment.PaymentIntent,"pi"){return inv,liveFailure()}
	return inv,nil
}
func (s *StripeLiveSmoke) readAssociation(ctx context.Context)error{inv,e:=s.invoice(ctx);if e!=nil{return e};s.intentID=inv.Payments.Data[0].Payment.PaymentIntent;return nil}
func (s *StripeLiveSmoke) state(ctx context.Context)(liveObject,liveObject,error){
	inv,e:=s.invoice(ctx);if e!=nil{return inv,liveObject{},e}
	var pi liveObject
	if inv.Payments.Data[0].Payment.PaymentIntent!=s.intentID||s.request(ctx,"GET","payment_intents/"+s.intentID,"",nil,&pi)!=nil||pi.ID!=s.intentID||!s.validObject(pi,"B",5000)||pi.Metadata["osc_invoice"]!=s.invoiceID||len(pi.PaymentMethodTypes)!=1||pi.PaymentMethodTypes[0]!="card"||pi.AmountReceived<0||pi.AmountReceived>5000{return inv,pi,liveFailure()}
	return inv,pi,nil
}

func (s *StripeLiveSmoke) summary(status string)LiveSmokeSummary{
	amount:=int64(5000);if s.options.Phase=="A"||s.originalA{amount=50}
	return LiveSmokeSummary{Phase:s.options.Phase,Status:status,AmountMinor:amount,Currency:"USD",Livemode:true,OwnerActionNeeded:status=="REFUND_UNRESOLVED"||status=="REFUND_UNRESOLVED_MANUAL"}
}

func (s *StripeLiveSmoke) Execute(ctx context.Context)(LiveSmokeSummary,error){
	if _,e:=s.now(ctx);e!=nil{return s.summary("PENDING"),liveFailure()}
	if s.options.Phase=="A"||s.originalA{
		f:=s.metadata("A");f.Set("amount","50");f.Set("currency","usd");f.Set("payment_method_types[0]","card")
		var pi liveObject
		if s.originalA {if s.request(ctx,"GET","payment_intents/"+s.intentID,"",nil,&pi)!=nil{return s.summary("PENDING"),liveFailure()}} else if s.request(ctx,"POST","payment_intents",s.key("a-create"),f,&pi)!=nil{return s.summary("PENDING"),liveFailure()}
		if !liveID(pi.ID,"pi")||!s.validObject(pi,"A",50){return s.summary("PENDING"),liveFailure()}
		if pi.Status=="succeeded"||pi.AmountReceived>0{return s.summary("REFUND_UNRESOLVED_MANUAL"),liveFailure()}
		ref:=pi.ID
		if s.request(ctx,"POST","payment_intents/"+ref+"/cancel",s.key("a-cancel"),nil,&pi)!=nil||pi.ID!=ref||!s.validObject(pi,"A",50)||pi.Status!="canceled"||pi.AmountReceived!=0{return s.summary("PENDING"),liveFailure()}
		return s.summary("CANCELED"),nil
	}
	if !liveID(s.intentID,"pi")||!liveID(s.invoiceID,"in")||s.bound.Validate()!=nil{return s.summary("PENDING"),liveFailure()}
	at,e:=s.now(ctx);if e!=nil{return s.summary("PENDING"),e};deadline:=at.Add(30*time.Minute)
	for{
		inv,pi,e:=s.state(ctx);if e!=nil{return s.summary("PENDING"),liveFailure()}
		if pi.Status=="succeeded" {
			v,e:=s.settle(ctx,pi);if e!=nil{return v,e}
			final,e:=s.invoice(ctx);if e!=nil||final.Status!="paid"||final.AmountPaid!=5000{return v,liveFailure()};return v,nil
		}
		if pi.Status=="processing" {return s.summary("PENDING"),liveFailure()}
		at,e=s.now(ctx);if e!=nil{return s.summary("PENDING"),e}
		if s.options.Phase=="R"||!at.Before(deadline){
			// Re-read immediately before void and after every void outcome. Never
			// infer no effect from the response, HTTP error, or timeout alone.
			inv,pi,e=s.state(ctx);if e!=nil{return s.summary("PENDING"),liveFailure()}
			if pi.Status=="succeeded"{v,_:=s.settle(ctx,pi);return v,liveFailure()}
			if pi.Status=="processing"{return s.summary("PENDING"),liveFailure()}
			if inv.Status=="open"&&(pi.Status=="requires_payment_method"||pi.Status=="canceled"){
				var ignored liveObject;_ = s.request(ctx,"POST","invoices/"+s.invoiceID+"/void",s.key("void"),nil,&ignored)
				inv,pi,e=s.state(ctx);if e!=nil{return s.summary("PENDING"),liveFailure()}
				if pi.Status=="succeeded"{v,_:=s.settle(ctx,pi);return v,liveFailure()}
			}
			if inv.Status=="void"&&pi.Status=="canceled"&&pi.AmountReceived==0{return s.summary("TIMED_OUT"),liveFailure()}
			return s.summary("PENDING"),liveFailure()
		}
		if s.pause(ctx)!=nil{return s.summary("PENDING"),liveFailure()}
	}
}

func (s *StripeLiveSmoke) settle(ctx context.Context,pi liveObject)(LiveSmokeSummary,error){
	if pi.AmountReceived!=5000{return s.summary("PENDING"),liveFailure()}
	// Financial ingestion errors cannot skip reimbursement of a succeeded charge.
	captureErr:=s.ingest(ctx,pi,false)
	r,status,e:=s.refund(ctx,pi)
	if e!=nil{return s.summary(status),liveFailure()}
	if s.ingest(ctx,r,true)!=nil||captureErr!=nil{return s.summary("REFUNDED"),liveFailure()}
	return s.summary("REFUNDED"),nil
}

func (s *StripeLiveSmoke) refunds(ctx context.Context)([]liveObject,error){
	var all []liveObject;after:="";seen:=map[string]bool{}
	for page:=0;page<100;page++{
		var list struct{Data []liveObject `json:"data"`;More bool `json:"has_more"`}
		q:=url.Values{"payment_intent":{s.intentID},"limit":{"100"}};if after!=""{q.Set("starting_after",after)}
		if s.request(ctx,"GET","refunds?"+q.Encode(),"",nil,&list)!=nil{return nil,liveFailure()}
		for _,r:=range list.Data{if !liveID(r.ID,"re")||seen[r.ID]||r.PaymentIntent!=s.intentID||r.Currency!="usd"||r.Amount<=0||r.Amount>5000{return nil,liveFailure()};seen[r.ID]=true;all=append(all,r)}
		if !list.More{return all,nil};if len(list.Data)==0{return nil,liveFailure()};after=list.Data[len(list.Data)-1].ID
	}
	return nil,liveFailure()
}

func (s *StripeLiveSmoke) refund(ctx context.Context,pi liveObject)(liveObject,string,error){
	all,e:=s.refunds(ctx);if e!=nil{return liveObject{},"PENDING",e}
	var succeeded []liveObject;var total int64;pending:=false
	for _,r:=range all{switch r.Status{case "succeeded":total+=r.Amount;succeeded=append(succeeded,r);case "failed","canceled":default:pending=true}}
	if total>0{if total!=pi.AmountReceived||len(succeeded)!=1||pending{return liveObject{},"REFUND_UNRESOLVED",liveFailure()};return succeeded[0],"REFUNDED",nil}
	if pending{return liveObject{},"PENDING",liveFailure()}
	attempt:=1
	if len(all)>0{if s.options.Phase!="R"||s.options.RefundAttempt<2{for _,r:=range all{if r.FailureReason=="lost_or_stolen_card"||r.FailureReason=="expired_or_canceled_card"{return liveObject{},"REFUND_UNRESOLVED_MANUAL",liveFailure()}};return liveObject{},"REFUND_UNRESOLVED",liveFailure()};attempt=s.options.RefundAttempt}
	for _,r:=range all{if r.Metadata["osc_smoke"]==s.options.RunID&&r.Metadata["osc_refund_attempt"]==strconv.Itoa(attempt){return liveObject{},"REFUND_UNRESOLVED",liveFailure()}}
	f:=s.metadata("B");f.Set("metadata[osc_invoice]",s.invoiceID);f.Set("metadata[osc_refund_attempt]",strconv.Itoa(attempt));f.Set("payment_intent",s.intentID);f.Set("amount",strconv.FormatInt(pi.AmountReceived,10))
	var r liveObject
	if s.request(ctx,"POST","refunds",s.key("refund:"+strconv.Itoa(attempt)),f,&r)!=nil{return r,"PENDING",liveFailure()}
	if !liveID(r.ID,"re")||r.PaymentIntent!=s.intentID||r.Currency!="usd"||r.Amount!=5000||r.Metadata["osc_smoke"]!=s.options.RunID||r.Metadata["osc_refund_attempt"]!=strconv.Itoa(attempt){return r,"PENDING",liveFailure()}
	at,e:=s.now(ctx);if e!=nil{return r,"PENDING",e};deadline:=at.Add(2*time.Minute)
	for{
		switch r.Status{case "succeeded":return r,"REFUNDED",nil;case "failed","canceled":return r,"REFUND_UNRESOLVED",liveFailure()}
		at,e=s.now(ctx);if e!=nil||!at.Before(deadline){return r,"PENDING",liveFailure()}
		if s.pause(ctx)!=nil{return r,"PENDING",liveFailure()}
		all,e=s.refunds(ctx);if e!=nil{return r,"PENDING",e};found:=false
		for _,v:=range all{if v.ID==r.ID{r=v;found=true}}
		if !found{return r,"PENDING",liveFailure()}
	}
}

// Refund remains a Payments-owned command; callers cannot choose a new amount,
// order, attempt identity or payment while recovering the smoke operation.
func (s *StripeLiveSmoke) Refund(ctx context.Context,r payment.PaymentOperation)(payment.PaymentIntentResult,error){
	if r.CreatePaymentIntent!=s.bound||r.ProviderRef!=s.intentID{return payment.PaymentIntentResult{},liveFailure()}
	_,pi,e:=s.state(ctx);if e!=nil||pi.Status!="succeeded"||pi.AmountReceived!=5000{return payment.PaymentIntentResult{},liveFailure()}
	v,_,e:=s.refund(ctx,pi);if e!=nil{return payment.PaymentIntentResult{},e}
	return payment.PaymentIntentResult{ProviderRef:s.intentID,RefundID:v.ID,Status:payment.StatusRefunded},nil
}
