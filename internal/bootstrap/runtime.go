// Responsibilities: compose optional configured persistence and, when the auth
// capability is configured, the staff login module on that same auth DSN.
// Dependencies: pgxadapter, identity composition and module Config ports; cmd/api
// calls this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	// Bound all capability pool probes together, including explicit DSNs.
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if config.Media.Root == "" {
		config.Media.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
	}
	privacyURL := os.Getenv("OPENSITE_PRIVACY_DATABASE_URL")
	if privacyURL != "" && (!filepath.IsAbs(config.Media.Root) || (os.Getenv("OPENSITE_PRIVACY_ROOT") != "" && filepath.Clean(os.Getenv("OPENSITE_PRIVACY_ROOT")) != filepath.Clean(config.Media.Root))) {
		return Config{}, nil, errors.New("privacy root must equal the configured media root")
	}
	var runners []*pgxadapter.QueryRunner
	var staff *identity.LoginModule
	closeRuntime := func() {
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
	if url := runtimeURL; url != "" {
		runner, err := pgxadapter.OpenQueryRunner(startup, url)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
		runners = append(runners, runner)
		config.Catalog.Run = runner.RunAuthorized
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
		config.Orders.Read = runner.RunAuthorizedSnapshot
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
