// Responsibilities: compose optional configured persistence without inventing
// authentication. Dependencies: pgxadapter and module Config ports; cmd/api calls
// this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	// Bound all capability pool probes together, including explicit DSNs.
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var runners []*pgxadapter.QueryRunner
	closeRuntime := func() {
		for _, runner := range runners {
			runner.Close()
		}
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
	// These pools verify separate deployment capabilities. Until identity/domain
	// consumers merge, no authentication is fabricated and business access denies.
	for _, capability := range []struct{ variable, kind, login string }{
		{"OPENSITE_AUTH_DATABASE_URL", "AUTH", "opensite_auth_login"},
		{"OPENSITE_DOMAIN_DATABASE_URL", "DOMAIN", "opensite_domain_login"},
	} {
		dsn, err := environmentDSN(capability.variable, capability.kind, capability.login)
		if err != nil {
			closeRuntime()
			return Config{}, nil, err
		}
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
