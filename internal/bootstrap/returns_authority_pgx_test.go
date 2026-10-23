//go:build pgx

// Responsibility: exercise refundReturn with the production coordinator LOGIN.
// Dependencies: disposable PG, quote paid-order fixture, staff auth and Fake PSP.
// Caller: foundation pgx gate. Invariant: authenticated refund and replay produce
// one provider refund and one journal without borrowing runtime privileges.
package bootstrap_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	ch "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

// Coordinator matrix: quote/unshipped and cart/signed-capture/shipped, with a
// negative original-payment version mismatch. Both paths use the dedicated LOGIN.
func TestReturnsCoordinatorAuthenticatedRefundPG(t *testing.T) {
	for _, source := range []string{"quote", "cart", "cart_mismatch"} {
		t.Run(source, func(t *testing.T) { returnsCoordinatorRefund(t, source) })
	}
}

type rmaMismatchPort struct{ *paymentfake.Fake }

func (p rmaMismatchPort) Refund(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	in.OrderVersion++
	return p.Fake.Refund(ctx, in)
}

func returnsCoordinatorRefund(t *testing.T, source string) {
	f := quoteOrdersDB(t, time.Hour)
	orderID := ""
	var start ch.Result
	var total int64 = 270
	if source == "quote" {
		paid, err := f.convertAndPay(t, f.buyerCtx, f.quote.ID, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		orderID = paid.OrderID
	} else {
		start = returnsCartPurchase(t, f)
		orderID = start.OrderID
		total = start.AmountMinor
	}
	raw := c02cHTTPRandom(t, 32)
	hash := sha256.Sum256(raw)
	var version int64
	var line string
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour')`, f.staff, hash[:]); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT o.version,l.id FROM app.orders o JOIN app.order_lines l ON l.tenant_id=o.tenant_id AND l.order_id=o.id WHERE o.tenant_id=$1 AND o.id=$2`, f.tenant, orderID).Scan(&version, &line)
	})
	const origin = "https://returns.example.test"
	for key, value := range map[string]string{
		"OPENSITE_DATABASE_URL":         loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_runtime_", "app_runtime"),
		"OPENSITE_AUTH_DATABASE_URL":    loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_auth_", "app_auth"),
		"OPENSITE_RETURNS_DATABASE_URL": loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "retgrant_service_", "app_returns_service"),
		"OSC_ADMIN_ORIGIN":              origin, "OSC_AUTH_SIGNING_KEY_HEX": hex.EncodeToString(quoteRandom(t)),
		"OPENSITE_IDEMPOTENCY_ROOT": t.TempDir(),
		"OPENSITE_FAKE_PSP":         "1", "OPENSITE_ENVIRONMENT": "test",
		"OPENSITE_FAKE_WEBHOOK_KEY": base64.RawURLEncoding.EncodeToString(f.signingKey),
	} {
		t.Setenv(key, value)
	}
	var provider payment.Port = f.fake
	if source == "cart_mismatch" {
		provider = rmaMismatchPort{f.fake}
	}
	cfg, closeRuntime, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{Payment: provider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	if cfg.ReturnsCoordinator.Runner == nil || cfg.Returns.Service == nil {
		t.Fatal("RETURNS_AUTHORITY_RED: production coordinator not composed")
	}
	called := false
	if err := cfg.ReturnsCoordinator.Runner.InTenant(f.ctx, f.tenant, func(context.Context, pgx.Tx) error { called = true; return nil }); err == nil || called {
		t.Fatal("unauthenticated coordinator callback admitted")
	}
	server, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	csrf := ""
	request := func(method, path, body, key string, version int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body)).WithContext(f.ctx)
		r.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: base64.RawURLEncoding.EncodeToString(raw)})
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", fmt.Sprintf(`"%d"`, version))
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		return w
	}
	session := request("GET", "/api/v1/auth/session", "", "", 0)
	var challenge struct {
		Token string `json:"csrf_token"`
	}
	if session.Code != 200 || json.Unmarshal(session.Body.Bytes(), &challenge) != nil || challenge.Token == "" {
		t.Fatalf("staff session: %d %s", session.Code, session.Body.String())
	}
	csrf = challenge.Token
	if source != "quote" {
		// Exactly the BFF path: signed completed fact, never trusted Confirm/Capture.
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled) VALUES($1,'fake',$2,'test',true)", f.tenant, "rma_"+f.tenant)
			return err
		})
		raw, _ := json.Marshal(map[string]any{"event_id": "capture_" + orderID, "account_id": "rma_" + f.tenant, "order_id": orderID, "order_version": start.OrderVersion, "provider_ref": start.Payment.ProviderRef, "status": "captured", "amount_minor": total, "currency": "USD"})
		stamp := fmt.Sprint(time.Now().Unix())
		mac := hmac.New(sha256.New, f.signingKey)
		mac.Write([]byte(stamp + "."))
		mac.Write(raw)
		receipt, err := cfg.Checkout.Events.Receive(f.ctx, raw, "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		if err != nil || receipt.Status != "processed" {
			t.Fatalf("capture fact: %+v %v", receipt, err)
		}
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT version FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, orderID).Scan(&version)
		})
	}
	rounds := 1
	if source != "quote" {
		rounds = 2
	}
	for part := 0; part < rounds; part++ {
		var shipmentLine string
		if source != "quote" {
			w := request("POST", "/api/v1/t/"+f.tenant+"/orders/"+orderID+"/shipments", fmt.Sprintf(`{"lines":[{"order_line_id":%q,"quantity":1}]}`, line), uuid.NewString(), version)
			var shipment httpapi.ShipmentRecord
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &shipment) != nil || len(shipment.Lines) != 1 {
				t.Fatalf("shipment: %d %s", w.Code, w.Body.String())
			}
			version = shipment.OrderVersion
			shipmentLine = shipment.Lines[0].Id.String()
		}
		body := fmt.Sprintf(`{"kind":"unshipped_cancel","reason":"CI coordinator regression","lines":[{"order_line_id":%q,"quantity":2}]}`, line)
		if source != "quote" {
			body = fmt.Sprintf(`{"kind":"shipment_return","reason":"CI cart regression","lines":[{"order_line_id":%q,"shipment_line_id":%q,"quantity":1}]}`, line, shipmentLine)
		}
		created := request("POST", "/api/v1/t/"+f.tenant+"/orders/"+orderID+"/returns", body, uuid.NewString(), version)
		var receipt httpapi.ReturnRecord
		if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &receipt) != nil {
			t.Fatalf("createReturn: %d %s", created.Code, created.Body.String())
		}
		if source != "quote" {
			w := request("POST", "/api/v1/t/"+f.tenant+"/returns/"+receipt.Id.String()+"/receive", `{"restock":true}`, uuid.NewString(), receipt.OrderVersion)
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
				t.Fatalf("receive: %d %s", w.Code, w.Body.String())
			}
		}
		path := "/api/v1/t/" + f.tenant + "/returns/" + receipt.Id.String() + "/refund"
		key := uuid.NewString()
		for attempt := range 2 {
			w := request("POST", path, `{}`, key, receipt.OrderVersion)
			if source == "cart_mismatch" {
				if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"payment_conflict"`) {
					t.Fatalf("binding mismatch: %d %s", w.Code, w.Body.String())
				}
				break
			}
			var result httpapi.ReturnRecord
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "refunded" || result.RefundMinor != fmt.Sprint(total/int64(rounds)) {
				t.Fatalf("RETURNS_AUTHORITY_RED: refundReturn attempt=%d status=%d body=%s", attempt, w.Code, w.Body.String())
			}
			version = result.OrderVersion
		}
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			var journals, refunded int64
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=$1 AND source_type='return' AND source_id=$2),refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$3`, f.tenant, receipt.Id.String(), orderID).Scan(&journals, &refunded); err != nil {
				return err
			}
			want := total * int64(part+1) / int64(rounds)
			events := 3 + part
			if source != "quote" {
				events = 2 + part
			}
			wantJournals := int64(1)
			if source == "cart_mismatch" {
				want = 0
				events = 1
				wantJournals = 0
			}
			if journals != wantJournals || refunded != want || len(f.fake.Events()) != events {
				return fmt.Errorf("refund conservation: journals=%d refunded=%d provider events=%d", journals, refunded, len(f.fake.Events()))
			}
			return nil
		})
		if source == "cart_mismatch" {
			break
		}
	}
}

// Seed shopping intent only; production Cart/Checkout calculate and seal money.
func returnsCartPurchase(t *testing.T, f *quoteOrderFixture) ch.Result {
	t.Helper()
	cartID := uuid.NewString()
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO app.carts(tenant_id,id,customer_id,currency) VALUES($1,$2,$3,'USD')", f.tenant, cartID, f.buyer); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity) VALUES($1,$2,$3,2)", f.tenant, cartID, f.sku)
		return err
	})
	service := checkout.New(checkout.Config{Run: f.runner.InTenant, Cart: checkout.NewSQLCart(cart.ReadIntent), Rules: checkout.NewSQLRules(checkout.RuleMetadata{Profiles: map[string]string{f.tenant: f.profile}, SKUs: map[string]map[string]checkout.SKUMetadata{f.tenant: {f.sku: {TaxCategory: "standard", WeightG: 100}}}}), Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: f.fake})
	actor, err := ch.NewAuthorizedActor(f.tenant, "customer", f.buyer)
	if err != nil {
		t.Fatal(err)
	}
	out, err := service.Start(f.ctx, actor, ch.StartInput{CartID: cartID, IdempotencyKey: uuid.NewString(), AcceptedTotal: ch.AcceptedTotal{AmountMinor: 1150, Currency: "USD"}, ShippingAddress: ch.ShippingAddress{Country: "US", Address1: "Test", City: "Test", PostalCode: "10000"}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
