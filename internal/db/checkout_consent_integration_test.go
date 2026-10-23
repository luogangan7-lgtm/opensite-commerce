//go:build pgx

// Responsibility: server consent regression witnesses against real Checkout PG.
// Dependencies: checkout PG fixture and public contract; caller: foundation CI.
// Invariants: no order/provider effect without matching explicit accepted money.
package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestCheckoutPGChangedTotalConsent(t *testing.T) {
	for _, accepted := range []string{`null`, `{"amount_minor":159,"currency":"USD"}`, `{"amount_minor":160,"currency":"EUR"}`} {
		t.Run(accepted, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 5, 1)
			input := f.input()
			raw, _ := json.Marshal(input)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			fields["accepted_total"] = json.RawMessage(accepted)
			raw, _ = json.Marshal(fields)
			var in checkoutcontract.StartInput
			if err := json.Unmarshal(raw, &in); err != nil {
				t.Fatal(err)
			}
			in.IdempotencyKey = input.IdempotencyKey
			want := checkoutcontract.ErrTotalChanged
			if accepted == `null` {
				want = checkoutcontract.ErrInvalid
			}
			if _, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in); !errors.Is(err, want) {
				t.Fatalf("missing/stale/currency-mismatched buyer consent: %v", err)
			}
			if len(f.provider.fake.Events()) != 0 {
				t.Fatal("rejected consent reached provider")
			}
			var count int
			if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT count(*) FROM app.orders WHERE tenant_id=$1", f.tenant).Scan(&count)
			}); err != nil || count != 0 {
				t.Fatalf("rejected consent created order: %d %v", count, err)
			}
		})
	}
}

func TestCheckoutPGConsentReplayAndRepricing(t *testing.T) {
	f := newCheckoutPGFixture(t, 10, 1)
	in := f.input()
	first, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.InMaintenance(f.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE app.price_entries SET unit_amount=200 WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
	if err != nil || replay != first {
		t.Fatalf("same key/consent lost original payable: %#v %v", replay, err)
	}
	in.AcceptedTotal.AmountMinor = 270
	if _, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in); !errors.Is(err, checkoutcontract.ErrKeyReused) {
		t.Fatalf("changed consent reused key: %v", err)
	}
	in.IdempotencyKey += ":new"
	in.AcceptedTotal.AmountMinor = 160
	if _, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in); !errors.Is(err, checkoutcontract.ErrTotalChanged) {
		t.Fatalf("new attempt accepted stale money: %v", err)
	}
	in.AcceptedTotal.AmountMinor = 270
	current, err := f.service.Start(checkoutRequestContext(f.ctx), f.actor, in)
	if err != nil || current.AmountMinor != 270 {
		t.Fatalf("explicit new consent failed: %#v %v", current, err)
	}
	var snapshot []byte
	if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT address_snapshot FROM app.checkout_sessions WHERE tenant_id=$1 AND id=$2", f.tenant, current.CheckoutID).Scan(&snapshot)
	}); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		AcceptedTotal checkoutcontract.AcceptedTotal `json:"accepted_total"`
	}
	if json.Unmarshal(snapshot, &receipt) != nil || receipt.AcceptedTotal != in.AcceptedTotal {
		t.Fatalf("missing durable consent receipt: %s", snapshot)
	}
	fact := f.event(current, "authorized")
	if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), fact); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), fact); err != nil {
		t.Fatal(err)
	}
	w := f.witness(t, current.OrderID)
	if w.cash != 270 || w.journals != 1 {
		t.Fatalf("capture replay duplicated money: %#v", w)
	}
}

func TestCheckoutPGHTTPConsentConflict(t *testing.T) {
	for _, accepted := range []string{`{"amount_minor":159,"currency":"USD"}`, `{"amount_minor":160,"currency":"EUR"}`} {
		f := newCheckoutPGFixture(t, 5, 1)
		in := f.input()
		raw, _ := json.Marshal(in)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		fields["accepted_total"] = json.RawMessage(accepted)
		raw, _ = json.Marshal(fields)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/t/"+f.tenant+"/checkout", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", checkoutHTTPOrigin)
		request.Header.Set("X-CSRF-Token", "fixture")
		request.Header.Set("Idempotency-Key", in.IdempotencyKey)
		response := httptest.NewRecorder()
		checkoutHTTPRouter(t, f).ServeHTTP(response, request)
		checkoutHTTPProblem(t, response, 409, "checkout_total_changed")
	}
}

func TestCheckoutPGLegacyOrForgedConsentCannotCapture(t *testing.T) {
	for _, snapshot := range []string{`{}`, `{"accepted_total":{"amount_minor":269,"currency":"USD"}}`, `{"accepted_total":{"amount_minor":270,"currency":"EUR"}}`} {
		a := newCheckoutAdoptionPG(t)
		id := uuid.NewString()
		if err := a.f.run(a.f.ctx, a.f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,source_kind,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
			 VALUES($1,$2,'customer',$3,'legacy-consent',sha256('legacy'::bytea),'cart',$4,1,$5,$6::jsonb,'USD',270,200,20,50,1)`, a.f.tenant, id, a.f.actor.ID(), uuid.NewString(), a.order.ID, snapshot)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "UPDATE app.checkout_sessions SET phase='ready',provider_ref='legacy-intent',start_result='{}' WHERE tenant_id=$1 AND id=$2", a.f.tenant, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		event := a.f.event(checkoutcontract.Result{OrderID: a.order.ID, OrderVersion: 1, AmountMinor: 270, Currency: "USD", Payment: checkoutcontract.Intent{ProviderRef: "legacy-intent"}}, "authorized")
		before := a.f.witness(t, a.order.ID)
		if _, err := a.f.service.Confirm(checkoutRequestContext(a.f.ctx), event); !errors.Is(err, checkoutcontract.ErrTotalChanged) {
			t.Fatalf("capture admitted legacy/forged consent: %v", err)
		}
		if a.f.provider.captures.Load() != 0 {
			t.Fatal("consent failure reached Capture")
		}
		if after := a.f.witness(t, a.order.ID); after != before || after.phase != "ready" {
			t.Fatalf("rejected Capture changed persisted state: %+v", after)
		}
	}
}

// Legacy snapshots cannot acquire a new capture claim, but an already durable
// claim must recover the original provider key and preserve permanent receipts.
func TestCheckoutPGConsentLegacyRecoveryAndPaidReplay(t *testing.T) {
	for _, status := range []payment.PaymentStatus{payment.StatusAuthorized, payment.StatusCaptured} {
		t.Run(string(status), func(t *testing.T) {
			a := newCheckoutAdoptionPG(t)
			f, id := a.f, uuid.NewString()
			request := payment.CreatePaymentIntent{TenantID: f.tenant, OrderID: a.order.ID, OrderVersion: 1, Amount: money.Money{Amount: 270, Currency: "USD"}, IdempotencyKey: "checkout:v1:" + id + ":create"}
			intent, err := f.provider.CreateIntent(checkoutRequestContext(f.ctx), request)
			if err != nil {
				t.Fatal(err)
			}
			start := checkoutcontract.Result{CheckoutID: id, OrderID: a.order.ID, State: "ready", AmountMinor: 270, Currency: "USD", OrderVersion: 1, Payment: checkoutcontract.Intent{ProviderRef: intent.ProviderRef, Status: intent.Status}}
			event := f.event(start, status)
			raw, _ := json.Marshal(start)
			if err := f.run(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,source_kind,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
				 VALUES($1,$2,'customer',$3,'legacy-recovery',sha256('legacy'::bytea),'cart',$4,1,$5,'{}','USD',270,200,20,50,1)`, f.tenant, id, f.actor.ID(), uuid.NewString(), a.order.ID)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, "UPDATE app.checkout_sessions SET phase='ready',provider_ref=$3,start_result=$4 WHERE tenant_id=$1 AND id=$2", f.tenant, id, intent.ProviderRef, raw)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, "UPDATE app.checkout_sessions SET phase='confirming',claim_kind='payment',claim_key=$3 WHERE tenant_id=$1 AND id=$2", f.tenant, id, event.EventID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			request.IdempotencyKey = "checkout:v1:" + id + ":capture"
			if _, err := f.provider.Capture(checkoutRequestContext(f.ctx), payment.PaymentOperation{CreatePaymentIntent: request, ProviderRef: intent.ProviderRef}); err != nil {
				t.Fatal(err)
			}
			before := f.witness(t, a.order.ID)
			if before.phase != "confirming" || before.journals != 0 || before.paidEvents != 0 {
				t.Fatalf("legacy uncertain witness: %+v", before)
			}
			foreign := event
			foreign.EventID += ":other-claim"
			if _, err := f.service.Confirm(checkoutRequestContext(f.ctx), foreign); !errors.Is(err, checkoutcontract.ErrConflict) {
				t.Fatalf("recovery changed claim key: %v", err)
			}
			if w := f.witness(t, a.order.ID); w != before || f.provider.captures.Load() != 1 {
				t.Fatalf("foreign claim changed money: %+v", w)
			}
			paid, err := f.service.Confirm(checkoutRequestContext(f.ctx), event)
			if err != nil || paid.State != "paid" {
				t.Fatalf("legacy recovery: %+v %v", paid, err)
			}
			w := f.witness(t, a.order.ID)
			if w.cash != 270 || w.journals != 1 || w.paidEvents != 1 || w.reserved != 0 || w.debits != w.credits {
				t.Fatalf("recovered money: %+v", w)
			}
			captures := f.provider.captures.Load()
			for _, replay := range []payment.VerifiedEvent{event, f.event(start, status)} {
				got, err := f.service.Confirm(checkoutRequestContext(f.ctx), replay)
				if err != nil || got != paid {
					t.Fatalf("legacy paid replay: %+v %v", got, err)
				}
			}
			if got := f.witness(t, a.order.ID); got != w || f.provider.captures.Load() != captures {
				t.Fatalf("paid replay changed durable money: %+v", got)
			}
		})
	}
}

func TestCheckoutPGHTTPConsentInvalid(t *testing.T) {
	for _, accepted := range []string{"", `null`, `{}`, `{"amount_minor":0,"currency":"USD"}`, `{"amount_minor":160,"currency":"usd"}`, `"wrong-shape"`, `[]`, `{"amount_minor":160,"currency":"USD","accepted":true}`, `{"Amount_minor":160,"currency":"USD"}`} {
		t.Run(accepted, func(t *testing.T) {
			f := newCheckoutPGFixture(t, 5, 1)
			in := f.input()
			raw, _ := json.Marshal(in)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if accepted == "" {
				delete(fields, "accepted_total")
			} else {
				fields["accepted_total"] = json.RawMessage(accepted)
			}
			raw, _ = json.Marshal(fields)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/t/"+f.tenant+"/checkout", strings.NewReader(string(raw)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", checkoutHTTPOrigin)
			req.Header.Set("X-CSRF-Token", "fixture")
			req.Header.Set("Idempotency-Key", in.IdempotencyKey)
			res := httptest.NewRecorder()
			checkoutHTTPRouter(t, f).ServeHTTP(res, req)
			checkoutHTTPProblem(t, res, 422, "validation_failed")
			if w := f.provider.fake.Events(); len(w) != 0 {
				t.Fatalf("invalid consent reached provider: %+v", w)
			}
			var count int
			if err := f.runner.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT count(*) FROM app.orders WHERE tenant_id=$1", f.tenant).Scan(&count)
			}); err != nil || count != 0 {
				t.Fatalf("invalid consent persisted order: %d %v", count, err)
			}
		})
	}
}
