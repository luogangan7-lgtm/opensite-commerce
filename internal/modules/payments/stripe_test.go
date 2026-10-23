// Responsibility: fail-closed Stripe key/consent/signature/HTTP regression probes.
// Dependencies: env fixtures and an in-memory RoundTripper, never real credentials.
// Caller: foundation and stripe-sandbox CI; all provider errors stay bounded.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type stripeTransport func(*http.Request) (*http.Response, error)

func (f stripeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func stripeTestConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("OPENSITE_ENVIRONMENT", "test")
	t.Setenv("STRIPE_TEST_WEBHOOK_SECRET", "whsec_isolated_unit_fixture_never_real_secret")
	t.Setenv("STRIPE_TEST_ACCOUNT_ID", "acct_fixture")
	t.Setenv("STRIPE_TEST_PAYMENT_METHOD", "pm_card_visa")
	return Config{Run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
		return fn(ctx, nil, "10000000-0000-4000-8000-000000000001", time.Now())
	}, Apply: func(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error) {
		return false, errors.New("no fact expected")
	}, Admit: func(context.Context, payment.CreatePaymentIntent, bool) error { return nil }}
}
func TestStripeRejectLiveBeforeNetwork(t *testing.T) {
	c := stripeTestConfig(t)
	old := http.DefaultTransport
	calls := 0
	http.DefaultTransport = stripeTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("network forbidden") })
	t.Cleanup(func() { http.DefaultTransport = old })
	defer func() {
		if calls != 0 {
			t.Fatal("rejected key reached network")
		}
	}()
	for _, key := range []string{"sk_live_unit_fixture", "rk_live_unit_fixture", "", "rk_test_unit_fixture"} {
		t.Setenv("STRIPE_TEST_SECRET_KEY", key)
		s, err := NewStripe(c)
		if err == nil || s != nil || (key != "" && strings.Contains(err.Error(), key)) {
			t.Fatal("unsafe key admission or credential disclosure")
		}
	}
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	t.Setenv("OPENSITE_ENVIRONMENT", "production")
	if _, err := NewStripe(c); err == nil {
		t.Fatal("production accepted sandbox")
	}
}
func TestStripeConsentAndBoundedTransport(t *testing.T) {
	c := stripeTestConfig(t)
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	deny := true
	c.Admit = func(context.Context, payment.CreatePaymentIntent, bool) error {
		if deny {
			return errors.New("cart consent required")
		}
		return nil
	}
	s, err := NewStripe(c)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.client.Transport = stripeTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.stripe.com" || r.URL.Scheme != "https" || r.Header.Get("Idempotency-Key") == "" {
			t.Fatal("provider URL/idempotency contract")
		}
		_ = r.ParseForm()
		if r.Form.Get("amount") != "1150" || r.Form.Get("currency") != "usd" || r.Form.Get("payment_method") != "pm_card_visa" {
			t.Fatal("wire amount/currency/test method drift")
		}
		return &http.Response{StatusCode: 402, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private_secret_payload"}}`))}, nil
	})
	r := payment.CreatePaymentIntent{TenantID: "10000000-0000-4000-8000-000000000001", OrderID: "10000000-0000-4000-8000-000000000002", OrderVersion: 1, Amount: money.Money{Amount: 1150, Currency: "USD"}, IdempotencyKey: "attempt-1"}
	if _, err = s.CreateIntent(context.Background(), r); err == nil || calls != 0 {
		t.Fatal("missing consent reached network")
	}
	if _, err = s.Capture(context.Background(), payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"}); err == nil || calls != 0 {
		t.Fatal("capture bypassed consent")
	}
	deny = false
	_, err = s.CreateIntent(context.Background(), r)
	var pe *payment.PaymentError
	if calls != 1 || !errors.As(err, &pe) || pe.Code != payment.ErrorNonRetryable || pe.NoSideEffect || strings.Contains(err.Error(), "private_secret") {
		t.Fatal("unsafe decline/error mapping")
	}
	if stripeKey(r, "create") != stripeKey(r, "create") || stripeKey(r, "create") == stripeKey(r, "refund") {
		t.Fatal("operation identity collision")
	}
}

func TestStripeSignatureRejectsBeforeDatabase(t *testing.T) {
	c := stripeTestConfig(t)
	t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
	calls := 0
	c.Run = func(context.Context, string, func(context.Context, pgx.Tx, string, time.Time) error) error {
		calls++
		return errors.New("mapped transaction reached")
	}
	s, err := NewStripe(c)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"evt_fixture","type":"payment_intent.succeeded","livemode":false,"data":{"object":{"id":"pi_fixture","amount":1150,"currency":"usd","metadata":{"osc_order":"10000000-0000-4000-8000-000000000002","osc_version":"1"}}}}`)
	mac := hmac.New(sha256.New, s.config.Key)
	mac.Write([]byte("123."))
	mac.Write(raw)
	sig := "t=123,v1=" + hex.EncodeToString(mac.Sum(nil))
	for _, bad := range []string{"", "t=123,v1=00", "t=123,t=123,v1=" + hex.EncodeToString(mac.Sum(nil))} {
		if _, err = s.Receive(context.Background(), raw, bad); err == nil || calls != 0 {
			t.Fatal("invalid signature admitted")
		}
	}
	if _, err = s.Receive(context.Background(), append(append([]byte{}, raw...), ' '), sig); err == nil || calls != 0 {
		t.Fatal("raw-byte mutation admitted")
	}
	if _, err = s.Receive(context.Background(), raw, sig+",v1="+strings.Repeat("0", 64)); err == nil || calls != 1 {
		t.Fatal("valid rotating signature failed admission")
	}
}

func TestStripeOutboundAccountAuthority(t *testing.T) {
	for _, mapping := range []string{"missing", "disabled", "foreign", "matching"} {
		t.Run(mapping, func(t *testing.T) {
			c := stripeTestConfig(t)
			t.Setenv("STRIPE_TEST_SECRET_KEY", "sk_test_unit_fixture")
			committed, runs, calls := false, 0, 0
			c.Run = func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
				runs++
				if account != "acct_fixture" {
					t.Fatal("wrong account lookup")
				}
				if mapping == "missing" || mapping == "disabled" {
					return errors.New("private mapping detail")
				}
				tenant := "10000000-0000-4000-8000-000000000001"
				if mapping == "foreign" {
					tenant = "10000000-0000-4000-8000-000000000099"
				}
				err := fn(ctx, nil, tenant, time.Now())
				committed = err == nil
				return err
			}
			s, err := NewStripe(c)
			if err != nil {
				t.Fatal(err)
			}
			s.client.Transport = stripeTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if !committed {
					t.Fatal("network before mapped transaction committed")
				}
				return nil, errors.New("network fixture")
			})
			r := payment.CreatePaymentIntent{TenantID: "10000000-0000-4000-8000-000000000001", OrderID: "10000000-0000-4000-8000-000000000002", OrderVersion: 1, Amount: money.Money{Amount: 1150, Currency: "USD"}, IdempotencyKey: "attempt-1"}
			for _, op := range []string{"create", "capture", "cancel", "refund"} {
				committed, runs, calls = false, 0, 0
				in := payment.PaymentOperation{CreatePaymentIntent: r, ProviderRef: "pi_fixture"}
				switch op {
				case "create":
					_, err = s.CreateIntent(context.Background(), r)
				case "capture":
					_, err = s.Capture(context.Background(), in)
				case "cancel":
					_, err = s.Cancel(context.Background(), in)
				case "refund":
					_, err = s.Refund(context.Background(), in)
				}
				want := 0
				if mapping == "matching" {
					want = 1
				}
				if err == nil || runs != 1 || calls != want || strings.Contains(err.Error(), "private mapping") {
					t.Fatalf("%s: mapping=%s runs=%d HTTP=%d", op, mapping, runs, calls)
				}
			}
		})
	}
}
