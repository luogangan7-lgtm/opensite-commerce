// Responsibilities: compose optional configured persistence and, when the auth
// capability is configured, the staff login module on that same auth DSN.
// Dependencies: pgxadapter, identity composition and module Config ports; cmd/api
// calls this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cartcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	checkoutmodule "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/paymentfake"
	httpcheckout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	// Bound all capability pool probes together, including explicit DSNs.
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var runners []*pgxadapter.QueryRunner
	var staff *identity.LoginModule
	var customerRunner *pgxadapter.CustomerRunner
	closeRuntime := func() {
		if customerRunner != nil {
			customerRunner.Close()
		}
		if staff != nil {
			closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = staff.Close(closing)
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
	checkoutEnv, err := checkoutEnvironment(runtimeURL)
	if err != nil {
		return Config{}, nil, err
	}
	if url := runtimeURL; url != "" {
		runner, err := pgxadapter.OpenQueryRunner(startup, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.Catalog.Run = runner.RunAuthorized
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
		config.Orders.Read = runner.RunAuthorizedSnapshot
		if config.Cart == nil {
			config.Cart = cart.NewPostgres(cart.Config{Run: runner.RunAuthorized, Read: runner.RunAuthorizedSnapshot}, nil)
		}
		if checkoutEnv != nil && config.Checkout == nil {
			resolver, err := httpcheckout.NewGuestResolver(httpcheckout.GuestConfig{TenantID: checkoutEnv.tenantID, Origin: checkoutEnv.origin, Key: checkoutEnv.key, VerifyCart: func(ctx context.Context, actor checkoutcontract.AuthorizedActor, cartID string) error {
				return runner.RunCapabilitySnapshot(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
					return verifyGuestCartSnapshot(ctx, tx, actor, cartID)
				})
			}})
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
			verifier, err := httpcheckout.NewFakeHMACVerifier(checkoutEnv.fakeKey)
			if err != nil {
				closeRuntime()
				return Config{}, nil, err
			}
			config.Checkout = &httpcheckout.Config{Service: checkoutmodule.New(checkoutmodule.Config{Run: checkoutCommandRun(runner), Read: checkoutSnapshotRun(runner), Cart: checkoutmodule.NewSQLCart(cart.ReadIntent), SnapshotCart: checkoutmodule.NewSQLCartSnapshot(cart.ReadIntentSnapshot), Rules: checkoutmodule.NewSQLRules(checkoutEnv.metadata), Orders: orders.New(), Ledger: ledger.New(), Payment: paymentfake.New()}), Resolver: resolver, Origin: checkoutEnv.origin, FakeEnabled: true, Environment: checkoutEnv.environment, Verifier: verifier}
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
	if config.Media.Root == "" {
		config.Media.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
	}
	if root := os.Getenv("OPENSITE_IDEMPOTENCY_ROOT"); root != "" && config.CMS.Idempotency.Store == nil {
		store, err := idempotency.NewLocalResponseStore(root, contract.MaxWriteBytes)
		if err != nil {
			closeRuntime()
			return Config{}, nil, errors.New("command response storage is unavailable")
		}
		config.CMS.Idempotency.Store = store
	}
	if staffEnabled {
		staff, err = StaffLogin(ctx, authURL, origin, keyHex, proxies)
		if err != nil || staff == nil {
			closeRuntime()
			return Config{}, nil, errors.New("staff login startup failed")
		}
		config.Authenticator = staff.Authenticator()
		config.Routes = append(config.Routes, staff.Routes()...)
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
	return config, closeRuntime, nil
}

// verifyGuestCartSnapshot runs only inside a tenant-scoped capability snapshot.
// The login must assume its granted runtime role before the cart RLS read.
func verifyGuestCartSnapshot(ctx context.Context, tx pgx.Tx, actor checkoutcontract.AuthorizedActor, cartID string) error {
	guestHash, err := hex.DecodeString(actor.ID())
	if err != nil || len(guestHash) != 32 {
		return checkoutcontract.ErrForbidden
	}
	if err := db.New(tx).SetCartRuntimeRole(ctx); err != nil {
		return err
	}
	_, err = cart.ReadIntentSnapshot(ctx, tx, cartcontract.AuthorizedActor{TenantID: actor.TenantID(), GuestTokenHash: guestHash}, cartID)
	return err
}

type checkoutTransactionRunner interface {
	RunAuthorizedTx(context.Context, string, func(context.Context, pgx.Tx) error) error
	RunAuthorizedTxSnapshot(context.Context, string, func(context.Context, pgx.Tx) error) error
	RunCapabilityTx(context.Context, string, func(context.Context, pgx.Tx) error) error
}

func withCheckoutRuntimeRole(fn func(context.Context, pgx.Tx) error) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		if err := db.New(tx).SetCartRuntimeRole(ctx); err != nil {
			return err
		}
		return fn(ctx, tx)
	}
}

func checkoutCommandRun(r checkoutTransactionRunner) checkoutcontract.Run {
	return func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		if verifiedTenant, ok := httpcheckout.VerifiedFakePaymentTenant(ctx); ok {
			if verifiedTenant != tenant {
				return checkoutcontract.ErrForbidden
			}
			return r.RunCapabilityTx(ctx, tenant, withCheckoutRuntimeRole(fn))
		}
		return r.RunAuthorizedTx(ctx, tenant, withCheckoutRuntimeRole(fn))
	}
}

func checkoutSnapshotRun(r checkoutTransactionRunner) checkoutcontract.Run {
	return func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		return r.RunAuthorizedTxSnapshot(ctx, tenant, withCheckoutRuntimeRole(fn))
	}
}

type checkoutSettings struct {
	origin, tenantID, environment string
	key, fakeKey                  []byte
	metadata                      checkoutmodule.RuleMetadata
}

func checkoutEnvironment(runtimeURL string) (*checkoutSettings, error) {
	flag := os.Getenv("OPENSITE_FAKE_PSP")
	if flag == "" || flag == "0" {
		return nil, nil
	}
	if flag != "1" {
		return nil, errors.New("invalid Fake PSP flag")
	}
	c := &checkoutSettings{origin: os.Getenv("OPENSITE_STOREFRONT_ORIGIN"), tenantID: os.Getenv("OPENSITE_STOREFRONT_TENANT_ID"), environment: os.Getenv("OPENSITE_ENVIRONMENT")}
	if runtimeURL == "" || (c.environment != "test" && c.environment != "development") {
		return nil, errors.New("Fake PSP requires nonproduction runtime database")
	}
	var err error
	c.key, err = base64.RawURLEncoding.DecodeString(os.Getenv("OPENSITE_STOREFRONT_KEY"))
	if err != nil || len(c.key) < 32 {
		return nil, errors.New("invalid storefront key")
	}
	c.fakeKey, err = base64.RawURLEncoding.DecodeString(os.Getenv("OPENSITE_FAKE_WEBHOOK_KEY"))
	if err != nil || len(c.fakeKey) < 32 {
		return nil, errors.New("invalid Fake webhook key")
	}
	raw := os.Getenv("OPENSITE_CHECKOUT_RULE_METADATA")
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, errors.New("invalid checkout rule metadata")
	}
	if err := json.Unmarshal([]byte(raw), &c.metadata); err != nil {
		return nil, fmt.Errorf("invalid checkout rule metadata: %w", err)
	}
	if c.metadata.Profiles[c.tenantID] == "" || len(c.metadata.SKUs[c.tenantID]) == 0 {
		return nil, errors.New("checkout tenant rule metadata missing")
	}
	return c, nil
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
