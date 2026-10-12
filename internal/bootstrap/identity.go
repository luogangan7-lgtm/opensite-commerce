// Identity composition validates optional offline configuration and opens only
// the dedicated staff auth connection. cmd/api calls it; no handlers or business
// SQL live here. Partial credentials or malformed signing keys fail startup.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"net/netip"
	"strings"
	"time"
)

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
	if databaseURL == "" && origin == "" && keyHex == "" && proxyConfig == "" {
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
