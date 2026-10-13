// Responsibilities: compose optional configured persistence and, when the auth
// capability is configured, the staff login module on that same auth DSN.
// Dependencies: pgxadapter, identity composition and module Config ports; cmd/api
// calls this at startup. Each database uses its own login, and errors omit credentials.
package bootstrap

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func FromEnvironment(ctx context.Context, config Config) (Config, func(), error) {
	// Bound all capability pool probes together, including explicit DSNs.
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
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
	if config.Media.Root == "" {
		config.Media.Root = os.Getenv("OPENSITE_MEDIA_ROOT")
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
