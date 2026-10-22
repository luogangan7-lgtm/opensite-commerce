//go:build pgx

// Responsibility: gate the production quote composition through customer HTTP
// admission and signed completed payment facts. Dependencies: public module ports;
// invariants: real customer capability, real checkout, no test business writes.
package bootstrap_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer"
	cc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	oc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	pc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type quoteCustomerMailbox struct{ token string }

// A scoped staff fixture selects a real seeded admin; public seller GET still
// executes the module's live tenant/member permission checks in PostgreSQL.
// This fixture never authenticates buyer mutations or reads identity headers.
type quoteSellerReadAuth struct{ tenant, staff string }

func (a quoteSellerReadAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.OperationID != "getQuoteOrderConversion" || spec.Method != http.MethodGet || spec.Scope != "quotes:read" || spec.Access != httpapi.AdminOrAPIKey || r.PathValue("tenant_id") != a.tenant {
		return (httpapi.DenyAuthenticator{}).Authenticate(ctx, r, spec)
	}
	return qc.WithTrustedActor(ctx, qc.ActorIdentity{TenantID: a.tenant, StaffID: a.staff}), nil
}

func (m *quoteCustomerMailbox) Send(_ context.Context, _, _, purpose, token string) error {
	if purpose == "register" {
		m.token = token
	}
	return nil
}

func quoteRandom(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("fixture entropy unavailable")
	}
	return b
}

func quoteCustomer(t *testing.T, f *quoteOrderFixture) (cc.Service, []byte, *http.Cookie) {
	t.Helper()
	if os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL") == "" {
		pgxtest.Skip(t, "NOT_RUN: dedicated customer authentication capability DSN required")
	}
	runner, err := pgxadapter.OpenCustomerRunner(f.ctx, os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	secret := quoteRandom(t)
	mail := &quoteCustomerMailbox{}
	service, err := customer.NewPostgres(runner.InTenant, customer.Config{Mailer: mail, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	password := base64.RawURLEncoding.EncodeToString(quoteRandom(t))
	ip := netip.MustParseAddr("192.0.2.81")
	if err := service.Register(f.ctx, f.tenant, f.buyer+"@example.test", password, ip); err != nil {
		t.Fatal(err)
	}
	if mail.token == "" {
		t.Fatal("real registration did not deliver verification capability")
	}
	if err := service.Verify(f.ctx, f.tenant, mail.token); err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(f.ctx, f.tenant, f.buyer+"@example.test", password, ip, "")
	if err != nil {
		t.Fatal(err)
	}
	return service, secret, &http.Cookie{Name: customerhttp.SessionCookie, Value: session.Token}
}

// Use the same immutable DB account resolver and runtime role as payment PG
// tests. Only the fixture mapping is seeded; the real consumer owns all effects.
func (f *quoteOrderFixture) factReceiver(t *testing.T) (pc.Receiver, string) {
	t.Helper()
	account := "acct_" + uuid.NewString()
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled) VALUES($1,'fake',$2,'test',true)", f.tenant, account)
		return err
	})
	run := func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
		return f.runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var tenant string
			var at time.Time
			if err := tx.QueryRow(ctx, "SELECT tenant_id::text,at_time FROM app.resolve_fake_payment_account($1)", account).Scan(&tenant, &at); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			return fn(ctx, tx, tenant, at)
		})
	}
	applier, ok := f.checkout.(checkoutcontract.PaymentEventApplier)
	if !ok {
		t.Fatal("real checkout lacks completed-fact consumer")
	}
	receiver, err := payments.New(payments.Config{Key: f.signingKey, Run: run, Apply: applier.ApplyPaymentEvent})
	if err != nil {
		t.Fatal(err)
	}
	return receiver, account
}

// Capture through the actual external Fake once, then deliver its generated
// completed fact over HTTP. Application provider commands remain observable.
func (f *quoteOrderFixture) deliverFact(t *testing.T, account, orderID string) *httptest.ResponseRecorder {
	t.Helper()
	captured := false
	for _, event := range f.fake.Events() {
		if event.OrderID == orderID && event.Status == payment.StatusCaptured {
			captured = true
		}
	}
	if !captured {
		for _, event := range f.fake.Events() {
			if event.OrderID != orderID || event.Status != payment.StatusAuthorized {
				continue
			}
			_, err := f.fake.Capture(f.ctx, payment.PaymentOperation{CreatePaymentIntent: payment.CreatePaymentIntent{
				TenantID: event.TenantID, OrderID: event.OrderID, OrderVersion: event.OrderVersion,
				Amount: event.Amount, IdempotencyKey: "external-capture:" + orderID,
			}, ProviderRef: event.ProviderRef})
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	for _, event := range f.fake.Events() {
		if event.OrderID != orderID || event.Status != payment.StatusCaptured {
			continue
		}
		raw, err := json.Marshal(map[string]any{"event_id": event.EventID, "account_id": account, "order_id": event.OrderID, "order_version": event.OrderVersion, "provider_ref": event.ProviderRef, "status": string(event.Status), "amount_minor": event.Amount.Amount, "currency": event.Amount.Currency})
		if err != nil {
			t.Fatal(err)
		}
		var at time.Time
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at)
		})
		stamp := fmt.Sprint(at.Unix())
		mac := hmac.New(sha256.New, f.signingKey)
		_, _ = mac.Write([]byte(stamp + "."))
		_, _ = mac.Write(raw)
		request := httptest.NewRequest(http.MethodPost, checkouthttp.FakePattern, strings.NewReader(string(raw))).WithContext(quoteObserved(f.ctx))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Fake-Signature", "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		creates, captures, cancels := f.prepay.createCalls, f.prepay.captureCalls, f.prepay.cancelCalls
		response := httptest.NewRecorder()
		f.confirm.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			var receipt pc.Receipt
			if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.EventID != event.EventID || receipt.Status != "processed" {
				t.Fatalf("HTTP receipt is not the captured provider event: %s", response.Body.String())
			}
		}
		if f.prepay.createCalls != creates || f.prepay.captureCalls != captures || f.prepay.cancelCalls != cancels {
			t.Fatal("completed-fact HTTP delivery issued a provider command")
		}
		return response
	}
	t.Fatal("actual provider captured event absent")
	return nil
}

func TestQuoteOrdersPGProductionCompositionAdmissionAndConfirm(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	foreign := quoteOrdersDB(t, time.Hour)
	service, secret, cookie := quoteCustomer(t, f)
	const origin = "https://store.example.invalid"
	receiver, account := f.factReceiver(t)
	server, err := bootstrap.New(bootstrap.Config{Customer: service, CustomerOrigin: origin, CustomerSecret: secret, Authenticator: quoteSellerReadAuth{tenant: f.tenant, staff: f.staff},
		Checkout:        &checkouthttp.Config{Service: f.checkout, Origin: origin, FakeEnabled: true, Environment: "test", Events: receiver},
		QuoteConversion: &bootstrap.QuoteConversionConfig{Runner: f.runner, Quotes: quotes.New(), Orders: orders.NewQuoteCreator()}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirm = server.Handler
	sessionRequest := httptest.NewRequest("GET", origin+"/api/v1/t/"+f.tenant+"/customer/session", nil).WithContext(f.ctx)
	sessionRequest.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	server.Handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != 200 || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("real session challenge: %d", sessionResponse.Code)
	}
	const key = "production-composition-shared-raw-key"
	post := func(quoteID, csrf, requestOrigin, body string, sessionCookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", origin+"/api/v1/t/"+f.tenant+"/quotes/"+quoteID+"/orders", strings.NewReader(body)).WithContext(quoteObserved(f.ctx))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", requestOrigin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", key)
		// Untrusted company and staff hints must never choose the actor/company.
		r.Header.Set("X-Buyer-Company-ID", f.otherCompany)
		r.Header.Set("X-Staff-ID", f.staff)
		if sessionCookie != nil {
			r.AddCookie(sessionCookie)
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		return w
	}
	for _, negative := range []struct {
		name, quote, csrf, origin, body string
		cookie                          *http.Cookie
		status                          int
	}{
		{"staff-only", f.quote.ID, session.CSRF, origin, "{}", nil, 401},
		{"forged-session", f.quote.ID, session.CSRF, origin, "{}", &http.Cookie{Name: customerhttp.SessionCookie, Value: "forged"}, 401},
		{"csrf", f.quote.ID, "forged", origin, "{}", cookie, 403},
		{"origin", f.quote.ID, session.CSRF, "https://foreign.example.invalid", "{}", cookie, 403},
		{"foreign-quote", uuid.NewString(), session.CSRF, origin, "{}", cookie, 404},
		{"existing-foreign-tenant-quote", foreign.quote.ID, session.CSRF, origin, "{}", cookie, 404},
		{"company-body", f.quote.ID, session.CSRF, origin, `{"company_id":"` + f.otherCompany + `"}`, cookie, 400},
	} {
		t.Run(negative.name, func(t *testing.T) {
			w := post(negative.quote, negative.csrf, negative.origin, negative.body, negative.cookie)
			if w.Code != negative.status {
				t.Fatalf("admission: %d want %d", w.Code, negative.status)
			}
		})
	}
	if f.prepay.createCalls != 0 || f.prepay.captureCalls != 0 {
		t.Fatal("negative admission reached payment")
	}
	w := post(f.quote.ID, session.CSRF, origin, "{}", cookie)
	var receipt struct {
		QuoteID     string `json:"quote_id"`
		OrderID     string `json:"order_id"`
		State       string `json:"state"`
		ProviderRef string `json:"provider_ref"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.OrderID == "" || receipt.QuoteID != f.quote.ID || receipt.State != "pending_payment" || receipt.ProviderRef == "" {
		t.Fatalf("typed conversion: %d %s", w.Code, w.Body.String())
	}
	if f.prepay.captureCalls != 0 {
		t.Fatal("conversion captured before signed Confirm")
	}
	assertRef := func(response *httptest.ResponseRecorder, wantStatus int, state string) {
		t.Helper()
		var projection struct {
			OrderID     string `json:"order_id"`
			State       string `json:"state"`
			ProviderRef string `json:"provider_ref"`
		}
		if response.Code != wantStatus || json.Unmarshal(response.Body.Bytes(), &projection) != nil || projection.OrderID != receipt.OrderID || projection.State != state || projection.ProviderRef != receipt.ProviderRef {
			t.Fatalf("durable provider ref replay: status=%d projection=%+v", response.Code, projection)
		}
	}
	sellerStatus := func(state string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, origin+"/api/v1/t/"+f.tenant+"/quotes/"+f.quote.ID+"/order-conversion", nil).WithContext(f.ctx)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		assertRef(response, http.StatusOK, state)
	}
	assertRef(post(f.quote.ID, session.CSRF, origin, "{}", cookie), http.StatusCreated, "pending_payment")
	sellerStatus("pending_payment")
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 0 || f.prepay.cancelCalls != 0 {
		t.Fatal("pending replay or seller status repeated provider effect")
	}
	f.assertEffects(t, receipt.OrderID, "pending_payment", 1, 0, 0, 100, 2)
	var admittedEvent string
	for i := 0; i < 2; i++ {
		confirmed := f.deliverFact(t, account, receipt.OrderID)
		var factReceipt pc.Receipt
		if confirmed.Code != 200 || json.Unmarshal(confirmed.Body.Bytes(), &factReceipt) != nil || factReceipt.EventID == "" || factReceipt.Status != "processed" {
			t.Fatalf("actual signed completed-fact replay: %d %s", confirmed.Code, confirmed.Body.String())
		}
		if admittedEvent != "" && factReceipt.EventID != admittedEvent {
			t.Fatal("completed-fact replay changed event identity")
		}
		admittedEvent = factReceipt.EventID
	}
	w = post(f.quote.ID, session.CSRF, origin, "{}", cookie)
	var replay struct {
		OrderID     string `json:"order_id"`
		State       string `json:"state"`
		ProviderRef string `json:"provider_ref"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.OrderID != receipt.OrderID || replay.State != "paid" || replay.ProviderRef != receipt.ProviderRef {
		t.Fatalf("durable HTTP replay: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		assertRef(post(f.quote.ID, session.CSRF, origin, "{}", cookie), http.StatusCreated, "paid")
		sellerStatus("paid")
	}
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 0 || f.prepay.cancelCalls != 0 {
		t.Fatal("completed-fact callback issued a provider command")
	}
	f.assertEffects(t, receipt.OrderID, "paid", 1, 1, 1, 98, 0)
	capturedFacts := 0
	for _, event := range f.fake.Events() {
		if event.OrderID == receipt.OrderID && event.Status == payment.StatusCaptured {
			capturedFacts++
		}
	}
	if capturedFacts != 1 {
		t.Fatalf("external capture effects after HTTP replay: %d", capturedFacts)
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var facts int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM app.payment_events WHERE tenant_id=$1 AND provider_account_id=$2 AND applied", f.tenant, account).Scan(&facts); err != nil {
			return err
		}
		if facts != 1 {
			return fmt.Errorf("completed-fact replay stored %d applied facts", facts)
		}
		_, err := tx.Exec(ctx, `UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=$1 AND customer_id=$2`, f.tenant, f.buyer)
		return err
	})
	if denied := post(f.quote.ID, session.CSRF, origin, "{}", cookie); denied.Code != 404 {
		t.Fatalf("membership loss replay admission: %d", denied.Code)
	}
	f.assertEffects(t, receipt.OrderID, "paid", 1, 1, 1, 98, 0)
}

func TestQuoteOrdersPGOrderKeyDoesNotCollideWithOrdinaryOrder(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	const raw = "ordinary-and-quote-same-raw-key"
	line, rate := uuid.NewString(), uuid.NewString()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := oc.CreateInput{ID: uuid.NewString(), IdempotencyKey: raw, Currency: "USD", Buyer: oc.Buyer{CustomerID: f.buyer},
		Lines: []oc.Line{{ID: line, SKUID: f.sku, SKU: "QUOTE-ORDER-SKU", Quantity: 2,
			Price: pricing.Result{Unit: money.Money{Amount: 100, Currency: "USD"}, Line: money.Money{Amount: 200, Currency: "USD"}, PriceListID: f.priceList, MinQuantity: 1},
			Tax:   tax.LineResult{ID: line, Amounts: tax.Amounts{Net: 200, Tax: 20, Gross: 220}, Snapshot: tax.Snapshot{Currency: "USD", ZoneID: rate, RateID: rate, Category: "standard", BasisPoints: 1000, ValidFrom: &start, Rounding: "half_up", Reason: "rate"}}}},
		Shipping: shipping.Result{Charge: money.Money{Amount: 50, Currency: "USD"}, ZoneID: rate, RateID: rate, Method: "fixed"}}
	var ordinary oc.Order
	if err := f.run(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ordinary, err = orders.New().Create(ctx, tx, f.tenant, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	converted, err := f.convertAndPay(t, f.buyerCtx, f.quote.ID, raw)
	if err != nil || converted.OrderID == ordinary.ID || converted.IdempotencyKey != raw || converted.State != "paid" {
		t.Fatalf("ordinary/quote raw key collision: %+v %v", converted, err)
	}
	if err := f.run(func(ctx context.Context, tx pgx.Tx) error {
		replay, err := orders.New().Create(ctx, tx, f.tenant, in)
		if err == nil && replay.ID != ordinary.ID {
			return fmt.Errorf("ordinary replay changed order")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.app.Convert(f.buyerCtx, f.quote.ID, raw); err != nil || replay.OrderID != converted.OrderID {
		t.Fatalf("quote replay: %+v %v", replay, err)
	}
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		var count, conversions, journals, paid, held, hand int64
		var ordinaryKey, receiptKey, checkoutKey string
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.orders WHERE tenant_id=$1),(SELECT count(*) FROM app.quote_conversions WHERE tenant_id=$1),(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1),(SELECT count(*) FROM app.outbox_events WHERE tenant_id=$1 AND event_type='order.paid'),reserved,on_hand FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2`, f.tenant, f.sku).Scan(&count, &conversions, &journals, &paid, &held, &hand); err != nil {
			return err
		}
		if count != 2 || conversions != 1 || journals != 1 || paid != 1 || held != 2 || hand != 98 {
			return fmt.Errorf("ordinary/quote effects %d/%d/%d/%d/%d/%d", count, conversions, journals, paid, held, hand)
		}
		if err := tx.QueryRow(ctx, `SELECT creation_key FROM app.orders WHERE tenant_id=$1 AND id=$2`, f.tenant, ordinary.ID).Scan(&ordinaryKey); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT idempotency_key FROM app.quote_conversions WHERE tenant_id=$1 AND order_id=$2`, f.tenant, converted.OrderID).Scan(&receiptKey); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT request_key FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2`, f.tenant, converted.OrderID).Scan(&checkoutKey); err != nil {
			return err
		}
		if ordinaryKey != raw || receiptKey != raw || checkoutKey != "quotes.orders.v1:"+converted.OrderID {
			return fmt.Errorf("raw receipt/ordinary key or internal checkout namespace changed")
		}
		return nil
	})
	if f.prepay.createCalls != 1 || f.prepay.captureCalls != 1 {
		t.Fatal("collision replay repeated provider effect")
	}
}
