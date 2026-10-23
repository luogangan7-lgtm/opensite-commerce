// Responsibility: regress pending recovery and signed refund reconciliation.
// Dependencies: isolated Stripe transport and mapped-run sentinels; caller: CI.
// Invariants: pending is retryable; updated and dashboard facts reach admission.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func TestStripePendingRefundRetryable(t *testing.T) {
	c := stripeTestConfig(t)
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	s, err := NewStripe(c)
	if err != nil {
		t.Fatal(err)
	}
	r := payment.CreatePaymentIntent{TenantID: "10000000-0000-4000-8000-000000000001", OrderID: "10000000-0000-4000-8000-000000000002", OrderVersion: 1, Amount: money.Money{Amount: 300, Currency: "USD"}, IdempotencyKey: "pending-refund"}
	status := "pending"
	s.client.Transport = stripeTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"id":"pi_fixture","status":"succeeded","livemode":false,"amount":1150,"currency":"usd","latest_charge":{"amount_refunded":300},"metadata":{"osc_tenant":"10000000-0000-4000-8000-000000000001","osc_order":"10000000-0000-4000-8000-000000000002","osc_version":"1"}}`
		if req.URL.Path == "/v1/refunds" {
			if req.Method != http.MethodGet {
				t.Fatal("pending retry issued another refund")
			}
			body = `{"data":[{"id":"re_fixture","status":"pending","amount":300,"metadata":{"osc_key":"` + stripeKey(r, "refund") + `"}}]}`
			body = strings.Replace(body, `"pending"`, `"`+status+`"`, 1)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	_, err = s.Refund(context.Background(), payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"})
	var pe *payment.PaymentError
	if !errors.As(err, &pe) || !pe.Retryable() || pe.NoSideEffect {
		t.Fatalf("pending refund must remain retryable without no-effect proof: %v", err)
	}
	for _, terminal := range []string{"canceled", "failed"} {
		status = terminal
		_, err = s.Refund(context.Background(), payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"})
		if !errors.As(err, &pe) || pe.Retryable() || !pe.NoSideEffect || pe.Code != payment.ErrorNonRetryable {
			t.Errorf("terminal %s refund must permanently release reservation: %v", terminal, err)
		}
	}
	status = "succeeded"
	result, err := s.Refund(context.Background(), payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"})
	if err != nil || result.Status != payment.StatusPartiallyRefunded {
		t.Fatalf("pending refund did not recover after provider success: %+v %v", result, err)
	}
}

func TestStripeRefundFactsReachMappedAdmission(t *testing.T) {
	for _, metadata := range []string{`"osc_order":"10000000-0000-4000-8000-000000000002","osc_version":"1"`, ""} {
		for _, kind := range []string{"refund.created", "refund.updated"} {
			t.Run(kind+"/"+metadata, func(t *testing.T) {
				c := stripeTestConfig(t)
				t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
				calls := 0
				c.Run = func(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
					calls++
					return errors.New("mapped admission sentinel")
				}
				s, err := NewStripe(c)
				if err != nil {
					t.Fatal(err)
				}
				raw := []byte(`{"id":"evt_refund","type":"` + kind + `","livemode":false,"data":{"object":{"id":"re_fixture","payment_intent":"pi_fixture","status":"succeeded","amount":300,"currency":"usd","metadata":{` + metadata + `}}}}`)
				mac := hmac.New(sha256.New, s.config.Key)
				mac.Write([]byte("123."))
				mac.Write(raw)
				_, err = s.Receive(context.Background(), raw, "t=123,v1="+hex.EncodeToString(mac.Sum(nil)))
				if err == nil || calls != 1 {
					t.Fatalf("refund fact silently skipped mapped admission: calls=%d error=%v", calls, err)
				}
			})
		}
	}
}

func TestStripeNewRefundTerminalNoEffect(t *testing.T) {
	for _, status := range []string{"canceled", "failed", "pending"} {
		t.Run(status, func(t *testing.T) {
			c := stripeTestConfig(t)
			t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
			s, err := NewStripe(c)
			if err != nil {
				t.Fatal(err)
			}
			r := payment.CreatePaymentIntent{TenantID: "10000000-0000-4000-8000-000000000001", OrderID: "10000000-0000-4000-8000-000000000002", OrderVersion: 1, Amount: money.Money{Amount: 300, Currency: "USD"}, IdempotencyKey: "terminal-refund"}
			posts := 0
			s.client.Transport = stripeTransport(func(req *http.Request) (*http.Response, error) {
				body := `{"id":"pi_fixture","status":"succeeded","livemode":false,"amount":1150,"currency":"usd","metadata":{"osc_tenant":"10000000-0000-4000-8000-000000000001","osc_order":"10000000-0000-4000-8000-000000000002","osc_version":"1"}}`
				if req.URL.Path == "/v1/refunds" {
					body = `{"data":[]}`
					if req.Method == http.MethodPost {
						posts++
						body = `{"id":"re_terminal","status":"` + status + `"}`
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			_, err = s.Refund(context.Background(), payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"})
			var pe *payment.PaymentError
			pending := status == "pending"
			if posts != 1 || !errors.As(err, &pe) || pe.Retryable() != pending || pe.NoSideEffect == pending {
				t.Fatalf("new %s refund error contract: posts=%d err=%v", status, posts, err)
			}
		})
	}
}

func TestStripePendingRefundDoesNotReconcile(t *testing.T) {
	c := stripeTestConfig(t)
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	calls := 0
	c.Run = func(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
		calls++
		return errors.New("PENDING_RECONCILIATION_RED")
	}
	s, err := NewStripe(c)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"evt_pending","type":"refund.created","livemode":false,"data":{"object":{"id":"re_pending","payment_intent":"pi_pending","status":"pending","amount":300,"currency":"usd","metadata":{"osc_order":"10000000-0000-4000-8000-000000000002","osc_version":"1"}}}}`)
	mac := hmac.New(sha256.New, s.config.Key)
	mac.Write([]byte("123."))
	mac.Write(raw)
	r, err := s.Receive(context.Background(), raw, "t=123,v1="+hex.EncodeToString(mac.Sum(nil)))
	if err != nil || calls != 0 || r.Status != "ignored" {
		t.Fatalf("PENDING_RECONCILIATION_RED: pending fact reached unmatched path: calls=%d receipt=%+v err=%v", calls, r, err)
	}
}
