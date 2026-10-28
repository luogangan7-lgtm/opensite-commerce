// Responsibilities: compose optional configured persistence and, when the auth
// capability is configured, the staff login module on that same auth DSN.
// Dependencies: pgxadapter, identity composition and module Config ports; cmd/api
// calls this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/tenant"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	if config.Subject == nil {
		config.Subject = staffSubject
	}
	stripeEnabled, stripeErr := stripeSandboxEnvironment()
	if stripeErr != nil {
		return Config{}, nil, stripeErr
	}
	fakeEnvironment, err := checkoutEnvironment(os.Getenv)
	if err != nil {
		return Config{}, nil, err
	}
	fakeEnabled, fakeKey, fakeErr := fakePaymentEnvironment()
	if fakeErr != nil {
		return Config{}, nil, fakeErr
	}
	if fakeEnvironment.enabled {
		// Both optional entrypoints share one account-mapped ingress. Refuse two
		// different verifier keys rather than silently choosing a signing authority.
		if fakeEnabled && !bytes.Equal(fakeKey, fakeEnvironment.key) {
			return Config{}, nil, errors.New("Fake checkout and payment signing keys conflict")
		}
		fakeEnabled, fakeKey = true, fakeEnvironment.key
	}
	if stripeEnabled && (fakeEnabled || fakeEnvironment.enabled) {
		return Config{}, nil, errors.New("Stripe sandbox and Fake configuration conflict")
	}
	// Bound all capability pool probes together, including explicit DSNs.
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if config.Media.Root == "" {
		config.Media.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
	}
	privacyURL := os.Getenv("OPENSITE_PRIVACY_DATABASE_URL")
	if privacyURL == "" {
		slog.Warn("privacy API unavailable", "missing_setting", "OPENSITE_PRIVACY_DATABASE_URL")
	}
	if os.Getenv("PRIVACY_EXPORT_WORKER_ENABLED") != "true" {
		slog.Warn("privacy export enqueue disabled", "missing_setting", "PRIVACY_EXPORT_WORKER_ENABLED=true")
	}
	if privacyURL != "" && (!filepath.IsAbs(config.Media.Root) || (os.Getenv("OPENSITE_PRIVACY_ROOT") != "" && filepath.Clean(os.Getenv("OPENSITE_PRIVACY_ROOT")) != filepath.Clean(config.Media.Root))) {
		return Config{}, nil, errors.New("privacy root must equal the configured media root")
	}
	var runners []*pgxadapter.QueryRunner
	var staff *identity.LoginModule
	var tenantAuth *tenant.Module
	var customerRunner *pgxadapter.CustomerRunner
	var quoteRuntime runtimeQuoteRunner
	var closeQuoteRuntime func()
	var closeInquiryPublic func()
	closeRuntime := func() {
		if closeInquiryPublic != nil {
			closeInquiryPublic()
		}
		if closeQuoteRuntime != nil {
			closeQuoteRuntime()
		}
		if customerRunner != nil {
			customerRunner.Close()
		}
		if staff != nil {
			closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = staff.Close(closing)
			cancel()
		}
		if tenantAuth != nil {
			closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = tenantAuth.Close(closing)
			cancel()
		}
		for _, runner := range runners {
			runner.Close()
		}
	}
	// Validate identity before any IO: a deployment with the auth capability but
	// no admin origin/signing key must refuse to start, not serve 404 logins.
	authURL, err := environmentDSN("OPENSITE_AUTH_DATABASE_URL", "AUTH", "opensite_auth_login")
	if err != nil {
		return Config{}, nil, err
	}
	staffEnabled, origin, keyHex, proxies, err := staffLoginEnvironment(authURL)
	if err != nil {
		return Config{}, nil, err
	}
	runtimeURL, err := environmentDSN("OPENSITE_DATABASE_URL", "RUNTIME", "opensite_runtime_login")
	if err != nil {
		return Config{}, nil, err
	}
	if (fakeEnabled || stripeEnabled) && runtimeURL == "" {
		return Config{}, nil, errors.New("Payment provider requires the runtime database")
	}
	paymentsURL, err := environmentDSN("OPENSITE_PAYMENTS_DATABASE_URL", "PAYMENTS", "opensite_payments_login")
	if err != nil {
		return Config{}, nil, err
	}
	if paymentsURL != "" {
		terminal, err := pgxadapter.OpenPaymentsRunner(startup, paymentsURL)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, terminal)
		config.PaymentTerminalRun = terminal.RunFakePaymentTx
		if stripeEnabled {
			config.PaymentTerminalRun = terminal.RunStripePaymentTx
		}
	}
	if url := runtimeURL; url != "" {
		runner, err := pgxadapter.OpenQueryRunner(startup, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		if stripeEnabled {
			if err := wireStripePayments(&config, runner); err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
		}
		if fakeEnabled {
			if err := wireFakePayments(&config, runner, fakeKey); err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
		}
		config.Catalog.Run = runner.RunAuthorized
		config.Pricing.Run = runner.RunAuthorized
		config.CatalogCSV.Run = runner.RunAuthorizedTx
		config.CatalogCSV.Service = catalog.NewCSV()
		config.CMS.Idempotency.Run = runner.RunAuthorizedTx
		if encoded := os.Getenv("OPENSITE_CMS_PREVIEW_KEY"); encoded != "" && config.CMS.Writer == nil {
			key, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(key) < 32 {
				closeRuntime()
				return Config{}, nil, errors.New("CMS preview key is invalid")
			}
			writer, err := publish.NewWriter(publish.WriteConfig{Run: runner.RunAuthorizedTx, ReadRun: runner.RunAuthorizedTxSnapshot,
				PreviewKey: key, PreviewRun: func(ctx context.Context, claims contract.PreviewClaims, fn func(context.Context, pgx.Tx) error) error {
					return runner.RunCapabilitySnapshot(ctx, claims.TenantID, fn)
				}})
			if err != nil {
				closeRuntime()
				return Config{}, nil, errors.New("CMS writer configuration is invalid")
			}
			config.CMS.Writer = writer
		}
		// Storefront serving refresh (ADR_STOREFRONT_SERVING 2.6): deployment-authorized
		// tenants only; without it serving rows degrade to their fallback, never to stale.
		list := os.Getenv("OPENSITE_SERVING_REFRESH_TENANTS")
		if list == "" && config.Logger != nil {
			config.Logger.Warn("OPENSITE_SERVING_REFRESH_TENANTS is empty: storefront serving rows are not refreshed and fall back to no purchasable SKUs")
		}
		if list != "" {
			refresher, err := publish.NewRefresher(publish.RefreshConfig{Run: runner.RunTrustedTx, Tenants: strings.Split(list, ","), Logger: config.Logger})
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
			refreshCtx, stopRefresh := context.WithCancel(context.WithoutCancel(ctx))
			done := make(chan struct{})
			go func() { defer close(done); refresher.Run(refreshCtx) }()
			previous := closeRuntime
			closeRuntime = func() { stopRefresh(); <-done; previous() }
		}
		if err := wireStorefrontCheckout(&config, runner); err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		config.Orders.Read = runner.RunAuthorizedSnapshot
		config.Fulfillment.Run = runner.RunAuthorizedTx
		config.Returns.Run = runner.RunAuthorizedTx
		if config.Notifications == nil {
			config.Notifications = notifications.New(runner.RunAuthorizedTx)
		}
		if config.Cart == nil {
			config.Cart = cart.NewPostgres(cart.Config{Run: runner.RunAuthorized, Read: runner.RunAuthorizedSnapshot}, nil)
		}
	}
	mediaURL, err := environmentDSN("OPENSITE_MEDIA_DATABASE_URL", "MEDIA", "opensite_media_login")
	if err != nil {
		closeRuntime()
		return Config{}, nil, err
	}
	if url := mediaURL; url != "" {
		runner, err := pgxadapter.OpenQueryRunner(startup, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.Media.Run = runner.RunAuthorized
	}
	// These pools verify separate deployment capabilities; the auth DSN is the one
	// derived above and reused by staff login below. Domain consumers are pending.
	domainURL, err := environmentDSN("OPENSITE_DOMAIN_DATABASE_URL", "DOMAIN", "opensite_domain_login")
	if err != nil {
		closeRuntime()
		return Config{}, nil, err
	}
	for _, dsn := range []string{authURL, domainURL} {
		if dsn == "" {
			continue
		}
		runner, err := pgxadapter.OpenQueryRunner(startup, dsn)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
	}
	if root := os.Getenv("OPENSITE_RESPONSE_ROOT"); root != "" {
		store, err := idempotency.NewLocalResponseStore(root, 1<<20)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		config.Privacy.Responses, config.CatalogCSV.Responses = store, store
	}
	if privacyURL != "" {
		runner, err := pgxadapter.OpenQueryRunner(startup, privacyURL)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		tokenKey, err := hex.DecodeString(os.Getenv("PRIVACY_TOKEN_KEY"))
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("invalid privacy token key")
		}
		config.Privacy.Run = runner.RunAuthorizedTx
		config.Privacy.Service, err = datarights.New(rights.Config{Run: runner.RunAuthorizedTx, Root: config.Media.Root, TokenKey: tokenKey})
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("privacy configuration unavailable")
		}
		// Explicit deployment flag means the separately configured privacy worker is
		// available. Without it exports fail before creating any unserviceable job.
		if os.Getenv("PRIVACY_EXPORT_WORKER_ENABLED") == "true" {
			config.Privacy.Enqueue, err = privacyEnqueue(os.Getenv("JOB_SIGNING_KEY"))
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
		}
	}
	// CSV import and privacy commands share one replay store; the media-root
	// fallback must not depend on privacy being configured (shipped compose).
	if config.CatalogCSV.Responses == nil && config.Media.Root != "" {
		store, err := idempotency.NewLocalResponseStore(filepath.Join(config.Media.Root, "command-responses"), 1<<20)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		config.Privacy.Responses, config.CatalogCSV.Responses = store, store
	}
	if root := os.Getenv("OPENSITE_IDEMPOTENCY_ROOT"); root != "" && (config.CMS.Idempotency.Store == nil || config.Fulfillment.Idempotency.Store == nil || config.Returns.Idempotency.Store == nil) {
		store, err := idempotency.NewLocalResponseStore(root, contract.MaxWriteBytes)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("command response storage is unavailable")
		}
		if config.CMS.Idempotency.Store == nil {
			config.CMS.Idempotency.Store = store
		}
		if config.Fulfillment.Idempotency.Store == nil {
			config.Fulfillment.Idempotency.Store = store
		}
		if config.Returns.Idempotency.Store == nil {
			config.Returns.Idempotency.Store = store
		}
	}
	if staffEnabled {
		staff, err = StaffLogin(ctx, authURL, origin, keyHex, proxies)
		if err != nil || staff == nil {
			closeRuntime()
			return Config{}, nil, errors.New("staff login startup failed")
		}
		tenantAuth, err = tenant.Open(startup, tenant.Config{DatabaseURL: authURL, ActorScopes: admittedSessionScopes})
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("tenant authorization startup failed")
		}
		staff.WithAuthorization(tenantAuth.Authorization())
		config.Authenticator = staff.Authenticator()
		config.Routes = append(config.Routes, tenantAuth.Routes()...)
		config.Routes = append(config.Routes, staff.Routes()...)
	}
	returnsURL, err := environmentDSN("OPENSITE_RETURNS_DATABASE_URL", "RETURNS", "opensite_returns_login")
	if err != nil {
		closeRuntime()
		return Config{}, nil, err
	}
	if returnsURL == "" {
		logger := config.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Info("returns coordinator unavailable: set OPENSITE_RETURNS_DATABASE_URL or OPENSITE_RETURNS_DB_PASSWORD")
	} else {
		runner, err := pgxadapter.OpenReturnsRunner(startup, returnsURL)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.ReturnsCoordinator.Runner = returnsCoordinatorRunner{runner}
	}
	customerURL, err := environmentDSN("OPENSITE_CUSTOMER_AUTH_DATABASE_URL", "CUSTOMER_AUTH", "opensite_customer_auth_login")
	if err != nil {
		closeRuntime()
		return Config{}, nil, err
	}
	if customerURL != "" && config.Customer == nil {
		origin := os.Getenv("OPENSITE_STOREFRONT_ORIGIN")
		secret, err := base64.RawURLEncoding.Strict().DecodeString(os.Getenv("OPENSITE_CUSTOMER_CSRF_SECRET"))
		if err != nil || len(secret) < 32 || len(secret) > 128 {
			closeRuntime()
			return Config{}, nil, errors.New("customer authentication configuration is invalid")
		}
		sender, err := notifications.SenderFromEnv(os.Getenv)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("customer notification configuration is invalid")
		}
		mailer, err := newCustomerMailer(sender, origin)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		customerRunner, err = pgxadapter.OpenCustomerRunner(startup, customerURL)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		config.Customer, err = customer.NewPostgres(customerRunner.InTenant, customer.Config{Mailer: mailer, Secret: secret})
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("customer authentication configuration is invalid")
		}
		config.CustomerOrigin, config.CustomerSecret = origin, secret
	}
	if fakeEnvironment.enabled && (runtimeURL == "" || config.Customer == nil) {
		closeRuntime()
		return Config{}, nil, errors.New("Fake checkout requires runtime and customer capabilities")
	}
	if runtimeURL != "" && (config.Quotes == nil || fakeEnvironment.enabled) {
		quoteRuntime, closeQuoteRuntime, err = openQuoteRuntime(startup, runtimeURL)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
	}
	config, err = composeFakeCheckout(config, quoteRuntime, fakeEnvironment)
	if err != nil {
		closeRuntime()
		return Config{}, nil, err
	}
	if config.Quotes == nil && runtimeURL != "" {
		config.Quotes = quotes.NewApplication(quoteRuntime)
	}
	// The same typed conversion/admission path also serves configured cart
	// checkout. Quote buyer access must not depend on the quote-only Fake toggle.
	if quoteRuntime.run != nil && config.QuoteConversion == nil && config.QuoteOrders == nil && config.Customer != nil && config.Checkout != nil && config.Checkout.Service != nil {
		config.QuoteConversion = &QuoteConversionConfig{Runner: quoteRuntime, Quotes: quotes.New(), Orders: orders.NewQuoteCreator()}
	}
	if dsn := os.Getenv("OSC_PUBLIC_DATABASE_URL"); dsn != "" && config.QuotesInquiryResolver == nil {
		var public *publish.Module
		public, closeInquiryPublic, err = publish.Open(startup, dsn)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("quote public resolver is unavailable or misconfigured")
		}
		config.QuotesInquiryResolver = quoteInquiryResolver(public)
	}
	wireReturns(&config)
	return config, closeRuntime, nil
}

// environmentDSN keeps passwords in the process environment, never command-line
// URLs. URL encoding preserves punctuation without granting connection options.
func environmentDSN(variable, kind, login string) (string, error) {
	if explicit := os.Getenv(variable); explicit != "" {
		return explicit, nil
	}
	password, configured := os.LookupEnv("OPENSITE_" + kind + "_DB_PASSWORD")
	if !configured {
		return "", nil
	}
	host := os.Getenv("OPENSITE_DB_HOST")
	database := os.Getenv("OPENSITE_DB")
	if database == "" {
		database = "opensite"
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`).MatchString(host) || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(database) || len(password) < 24 {
		return "", errors.New("database component configuration is invalid")
	}
	dsn := url.URL{Scheme: "postgresql", User: url.UserPassword(login, password), Host: host + ":5432", Path: "/" + database, RawQuery: "sslmode=disable"}
	return dsn.String(), nil
}

// returnsCoordinatorRunner lends authorized tenant transactions, never a pool.
// Trusted capture/account/payment composition remains the Returns consumer's job.
type returnsCoordinatorRunner struct{ runner *pgxadapter.QueryRunner }

func (r returnsCoordinatorRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	return r.runner.RunAuthorizedTx(ctx, tenant, fn)
}
