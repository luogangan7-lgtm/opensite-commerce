// Responsibility: opt in to the development/test checkout capability explicitly.
// Dependencies: neutral Fake, public checkout/orders/ledger and pgxadapter ports;
// FromEnvironment calls this before database IO. It owns no persistence or secrets.
// Invariants: no live provider or implicit Fake; incomplete/prod configuration fails.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type fakeCheckoutEnvironment struct {
	enabled     bool
	environment string
	key         []byte
}

func checkoutEnvironment(getenv func(string) string) (fakeCheckoutEnvironment, error) {
	out := fakeCheckoutEnvironment{environment: getenv("OPENSITE_ENVIRONMENT")}
	flag, encoded := getenv("OPENSITE_CHECKOUT_FAKE_ENABLED"), getenv("OPENSITE_CHECKOUT_FAKE_SIGNING_KEY")
	if flag == "" || flag == "false" {
		if encoded != "" {
			return out, errors.New("Fake checkout signing configuration requires explicit enablement")
		}
		return out, nil
	}
	if flag != "true" || out.environment != "test" && out.environment != "development" {
		return out, errors.New("Fake checkout requires explicit test/development enablement")
	}
	var err error
	out.key, err = base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(out.key) < 32 || len(out.key) > 128 {
		return out, errors.New("Fake checkout signing key is invalid")
	}
	out.enabled = true
	return out, nil
}

// Runtime service authority is distinct from HTTP URL admission: Confirm arrives
// from the signature verifier, while the checkout core checks durable binding.
// Quote admission instead needs a verified customer before the router's stamp.
type runtimeQuoteRunner struct{ run, snapshot checkoutcontract.Run }

func (r runtimeQuoteRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	if actor, err := qc.ActorFromContext(ctx); err == nil && actor.TenantID() == tenant {
		return r.run(ctx, tenant, fn)
	}
	if actor, ok := customercontract.ActorFromContext(ctx); ok && actor.TenantID == tenant && actor.CustomerID != "" {
		return r.snapshot(ctx, tenant, fn)
	}
	if admitted, err := httpapi.AuthorizedTenant(ctx); err == nil && admitted == tenant {
		return r.run(ctx, tenant, fn)
	}
	return qc.ErrUnauthorized
}

func composeFakeCheckout(config Config, runner runtimeQuoteRunner, env fakeCheckoutEnvironment) (Config, error) {
	if !env.enabled {
		return config, nil
	}
	if runner.run == nil || runner.snapshot == nil || config.Customer == nil || config.Checkout == nil || config.Checkout.Events == nil || config.Checkout.Service != nil || config.Checkout.Resolver != nil || config.QuoteConversion != nil || config.QuoteOrders != nil || config.QuotesBuyerAuthenticator != nil {
		return Config{}, errors.New("Fake checkout requires runtime/customer capabilities and excludes injected payment composition")
	}
	if bytes.Equal(env.key, config.CustomerSecret) {
		return Config{}, errors.New("Fake checkout requires an independent signing key")
	}
	// Adoption uses the sealed Orders snapshot; nil Cart/Rules deliberately expose
	// no cart-start readiness or unmodeled canonical catalog metadata.
	config.Payment = paymentfake.New()
	service := checkout.New(checkout.Config{Run: runner.run, Orders: orders.New(), Ledger: payments.RefundJournal{Ledger: ledger.New()}, Payment: config.Payment})
	// Preserve the mapped Payments receiver composed by wireFakePayments. Public
	// callbacks admit completed facts; the retired verifier cannot authorize them.
	config.Checkout.Service, config.Checkout.Origin = service, config.CustomerOrigin
	config.Checkout.Environment, config.Checkout.FakeEnabled = env.environment, true
	config.Quotes = quotes.NewApplication(runner)
	config.QuoteConversion = &QuoteConversionConfig{Runner: runner, Quotes: quotes.New(), Orders: orders.NewQuoteCreator()}
	return config, nil
}
