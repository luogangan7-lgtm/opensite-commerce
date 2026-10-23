// Responsibility: wire explicit nonproduction payment capabilities.
// Dependencies: checkout's fact consumer, Payments and the central mapped runner;
// caller: FromEnvironment. Staff/customer admission and provider IO are unchanged.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"os"
	"strings"
)

func stripeSandboxEnvironment() (bool, error) {
	provider := os.Getenv("OPENSITE_PAYMENT_PROVIDER")
	if provider == "" || provider == "fake" {
		return false, nil
	}
	if provider != "stripe-sandbox" {
		return false, errors.New("unsupported payment provider")
	}
	if env := os.Getenv("OPENSITE_ENVIRONMENT"); env != "test" && env != "development" {
		return false, errors.New("Stripe sandbox forbidden outside test/development")
	}
	if !strings.HasPrefix(os.Getenv("STRIPE_TEST_SECRET_KEY"), "sk_test_") {
		return false, errors.New("Stripe requires a test secret key")
	}
	return true, nil
}

func wireStripePayments(c *Config, r *pgxadapter.QueryRunner) error {
	consumer := checkout.New(checkout.Config{Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}}).(checkoutcontract.PaymentEventApplier)
	adapter, err := payments.NewStripe(payments.Config{TerminalRun: c.PaymentTerminalRun, Run: r.RunStripePaymentTx, Apply: consumer.ApplyPaymentEvent, Admit: func(ctx context.Context, in payment.CreatePaymentIntent, capture bool) error {
		if c.Checkout == nil || c.Checkout.Service == nil {
			return errors.New("Stripe cart consent unavailable")
		}
		admit, ok := c.Checkout.Service.(interface {
			AdmitRealPayment(context.Context, payment.CreatePaymentIntent, bool) error
		})
		if !ok {
			return errors.New("Stripe cart consent unavailable")
		}
		return admit.AdmitRealPayment(ctx, in, capture)
	}})
	if err != nil {
		return err
	}
	c.Payment, c.Stripe = adapter, adapter
	if c.Checkout == nil {
		c.Checkout = &checkouthttp.Config{}
	}
	return nil
}

// fakePaymentEnvironment centralizes the same OPENSITE_FAKE_PSP/environment gate
// used by storefront's Fake-only checkout composition; it runs before any IO.
func fakePaymentEnvironment() (bool, []byte, error) {
	flag := os.Getenv("OPENSITE_FAKE_PSP")
	if flag == "" || flag == "0" {
		return false, nil, nil
	}
	if flag != "1" {
		return false, nil, errors.New("invalid Fake payment toggle")
	}
	env := os.Getenv("OPENSITE_ENVIRONMENT")
	if env != "test" && env != "development" {
		return false, nil, errors.New("Fake payment is forbidden outside test/development")
	}
	key, e := base64.RawURLEncoding.Strict().DecodeString(os.Getenv("OPENSITE_FAKE_WEBHOOK_KEY"))
	if e != nil || len(key) < 32 || len(key) > 128 {
		return false, nil, errors.New("invalid isolated Fake webhook key")
	}
	return true, key, nil
}
func wireFakePayments(config *Config, runner *pgxadapter.QueryRunner, key []byte) error {
	consumer := checkout.New(checkout.Config{Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}}).(checkoutcontract.PaymentEventApplier)
	receiver, e := payments.New(payments.Config{TerminalRun: c.PaymentTerminalRun, Key: key, Run: runner.RunFakePaymentTx, Apply: consumer.ApplyPaymentEvent})
	if e != nil {
		return e
	}
	if config.Checkout == nil {
		config.Checkout = &checkouthttp.Config{}
	}
	config.Checkout.FakeEnabled = true
	config.Checkout.Environment = os.Getenv("OPENSITE_ENVIRONMENT")
	config.Checkout.Events = receiver
	return nil
}
