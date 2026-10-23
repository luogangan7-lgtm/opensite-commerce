// Identity composition validates optional offline configuration and opens only
// the dedicated staff auth connection. FromEnvironment calls it with the auth DSN
// it already derived; no handlers or business SQL live here. Partial or
// malformed identity configuration fails startup loudly; it never drops routes.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"net/netip"
	"os"
	"strings"
	"time"
)

// staffSubject adapts only the Router's sealed projection for CSV/privacy
// commands. API keys retain their key identity for idempotency and audit.
func staffSubject(ctx context.Context) (httpapi.Subject, error) {
	actor, err := httpapi.AuthorizedActor(ctx)
	if err != nil {
		return httpapi.Subject{}, err
	}
	return httpapi.Subject{Kind: actor.Kind, ID: actor.ID}, nil
}

// staffLoginEnvironment pairs the deployment's auth DSN (from FromEnvironment)
// with OSC_ADMIN_ORIGIN, OSC_AUTH_SIGNING_KEY_HEX and OSC_AUTH_TRUSTED_PROXIES.
// No identity setting at all means no staff module (e.g. storefront e2e); any
// identity setting requires the full set. Errors name variables, never values.
func staffLoginEnvironment(authDSN string) (enabled bool, origin, keyHex, proxies string, err error) {
	origin, keyHex, proxies = os.Getenv("OSC_ADMIN_ORIGIN"), os.Getenv("OSC_AUTH_SIGNING_KEY_HEX"), os.Getenv("OSC_AUTH_TRUSTED_PROXIES")
	if authDSN == "" && origin == "" && keyHex == "" && proxies == "" && os.Getenv("OSC_AUTH_SESSION_IDLE") == "" {
		return false, "", "", "", nil
	}
	var missing []string
	if authDSN == "" {
		missing = append(missing, "OPENSITE_AUTH_DATABASE_URL or OPENSITE_AUTH_DB_PASSWORD")
	}
	if origin == "" {
		missing = append(missing, "OSC_ADMIN_ORIGIN")
	}
	if keyHex == "" {
		missing = append(missing, "OSC_AUTH_SIGNING_KEY_HEX")
	}
	if len(missing) > 0 {
		return false, "", "", "", fmt.Errorf("staff login configuration is incomplete; set %s", strings.Join(missing, ", "))
	}
	if _, err = staffLoginConfig(authDSN, origin, keyHex, proxies); err != nil {
		return false, "", "", "", err
	}
	return true, origin, keyHex, proxies, nil
}

// StaffLogin retains the optional four-argument composition contract; the fifth
// argument is the explicit deployment proxy allowlist, never inferred from headers.
func StaffLogin(ctx context.Context, databaseURL, origin, keyHex string, trustedConfig ...string) (*identity.LoginModule, error) {
	proxyConfig := ""
	if len(trustedConfig) > 1 {
		return nil, errors.New("staff login configuration is invalid")
	}
	if len(trustedConfig) == 1 {
		proxyConfig = trustedConfig[0]
	}
	if databaseURL == "" && origin == "" && keyHex == "" && proxyConfig == "" && os.Getenv("OSC_AUTH_SESSION_IDLE") == "" {
		return nil, nil
	}
	cfg, err := staffLoginConfig(databaseURL, origin, keyHex, proxyConfig)
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return identity.OpenLogin(startup, databaseURL, cfg)
}

// staffLoginConfig validates all configuration before any connection is opened.
func staffLoginConfig(databaseURL, origin, keyHex, proxyConfig string) (identity.LoginConfig, error) {
	invalid := errors.New("staff login configuration is invalid")
	key, err := hex.DecodeString(keyHex)
	if databaseURL == "" || origin == "" || err != nil || len(key) != 32 {
		return identity.LoginConfig{}, invalid
	}
	cfg := identity.LoginConfig{Origin: origin, SigningKey: key}
	// The disposable browser witness uses the existing one-minute minimum;
	// deployments retain the default unless this explicit bound is configured.
	if raw := os.Getenv("OSC_AUTH_SESSION_IDLE"); raw != "" {
		idle, err := time.ParseDuration(raw)
		if err != nil || idle < time.Minute || idle > 12*time.Hour {
			return identity.LoginConfig{}, errors.New("OSC_AUTH_SESSION_IDLE is invalid")
		}
		cfg.IdleTimeout = idle
	}
	if proxyConfig == "" {
		return cfg, nil
	}
	for _, item := range strings.Split(proxyConfig, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return identity.LoginConfig{}, invalid
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, prefix)
	}
	return cfg, nil
}
