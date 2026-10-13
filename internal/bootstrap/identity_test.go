// Composition tests keep missing/partial credentials fail-closed before DB IO.
// Dependencies: only bootstrap; production auth authority is tested with real PG.
package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStaffSessionIdleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		invalid bool
	}{
		{"", 0, false}, {"1m", time.Minute, false}, {"30m", 30 * time.Minute, false},
		{"59s", 0, true}, {"13h", 0, true}, {"invalid-private-value", 0, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("OSC_AUTH_SESSION_IDLE", tc.raw)
			cfg, err := staffLoginConfig("secret-dsn", "https://admin.example.test", strings.Repeat("1", 64), "")
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "OSC_AUTH_SESSION_IDLE") || strings.Contains(err.Error(), tc.raw) {
					t.Fatal("invalid idle timeout must fail closed with variable name only")
				}
			} else if err != nil || cfg.IdleTimeout != tc.want {
				t.Fatalf("STAFF_IDLE_CONFIG_RED: timeout=%s want=%s err=%v", cfg.IdleTimeout, tc.want, err)
			}
		})
	}
}

func TestStaffLoginConfiguration(t *testing.T) {
	m, e := StaffLogin(context.Background(), "", "", "")
	if e != nil || m != nil {
		t.Fatal("optional staff module incorrectly enabled")
	}
	for _, tc := range []struct{ dsn, origin, key string }{{"secret-dsn", "https://admin.example.test", ""}, {"", "https://admin.example.test", strings.Repeat("1", 64)}, {"secret-dsn", "", strings.Repeat("1", 64)}, {"secret-dsn", "https://admin.example.test", "not-hex"}, {"secret-dsn", "https://admin.example.test", strings.Repeat("1", 32)}} {
		m, e = StaffLogin(context.Background(), tc.dsn, tc.origin, tc.key)
		if e == nil || m != nil || strings.Contains(e.Error(), "secret-dsn") {
			t.Fatal("unsafe partial configuration")
		}
	}
}

func TestStaffLoginTrustedProxyConfiguration(t *testing.T) {
	for _, raw := range []string{"", "192.0.2.0/24", " 192.0.2.0/24, 2001:db8::/32 "} {
		cfg, err := staffLoginConfig("secret-dsn", "https://admin.example.test", strings.Repeat("1", 64), raw)
		if err != nil {
			t.Fatalf("valid proxy configuration %q: %v", raw, err)
		}
		want := 0
		if raw != "" {
			want = strings.Count(raw, ",") + 1
		}
		if cfg.Origin != "https://admin.example.test" || len(cfg.SigningKey) != 32 {
			t.Fatal("credential configuration not wired")
		}
		if len(cfg.TrustedProxies) != want {
			t.Fatalf("proxy configuration not wired: got %v", cfg.TrustedProxies)
		}
		for i, prefix := range cfg.TrustedProxies {
			if prefix.String() != strings.TrimSpace(strings.Split(raw, ",")[i]) {
				t.Fatal("trusted prefix changed")
			}
			if !prefix.IsValid() || prefix != prefix.Masked() {
				t.Fatal("noncanonical trusted prefix")
			}
		}
	}
	for _, raw := range []string{" ", "192.0.2.1", "192.0.2.0/33", "192.0.2.1/24", "2001:db8::1/32", "fe80::%eth0/64", "::ffff:192.0.2.0/120", "192.0.2.0/24,", ",192.0.2.0/24", "192.0.2.0/24,,2001:db8::/32", "not-a-prefix"} {
		_, err := staffLoginConfig("secret-dsn", "https://admin.example.test", strings.Repeat("1", 64), raw)
		if err == nil || strings.Contains(err.Error(), "secret-dsn") || (strings.TrimSpace(raw) != "" && strings.Contains(err.Error(), raw)) {
			t.Fatalf("unsafe proxy configuration %q: %v", raw, err)
		}
	}
	if m, err := StaffLogin(context.Background(), "", "", "", "", ""); err == nil || m != nil {
		t.Fatal("ambiguous proxy configuration enabled")
	}
	if m, err := StaffLogin(context.Background(), "", "", "", ""); err != nil || m != nil {
		t.Fatal("empty optional proxy configuration enabled")
	}
	if m, err := StaffLogin(context.Background(), "", "", "", "192.0.2.0/24"); err == nil || m != nil {
		t.Fatal("proxy-only partial configuration enabled")
	}
}

// Compose supplies the auth capability; identity config must then be complete,
// so a missing origin/key fails startup loudly instead of omitting login routes.
func TestStaffLoginEnvironmentFailsLoudlyWhenIncomplete(t *testing.T) {
	key := strings.Repeat("1", 64)
	set := func(origin, keyHex, proxies string) {
		t.Setenv("OSC_ADMIN_ORIGIN", origin)
		t.Setenv("OSC_AUTH_SIGNING_KEY_HEX", keyHex)
		t.Setenv("OSC_AUTH_TRUSTED_PROXIES", proxies)
	}
	set("", "", "")
	if enabled, _, _, _, err := staffLoginEnvironment(""); enabled || err != nil {
		t.Fatal("no identity configuration must leave staff login disabled without error")
	}
	for _, tc := range []struct {
		dsn, origin, key, want string
	}{
		{"secret-dsn", "", "", "OSC_ADMIN_ORIGIN, OSC_AUTH_SIGNING_KEY_HEX"},
		{"secret-dsn", "https://admin.example.test", "", "OSC_AUTH_SIGNING_KEY_HEX"},
		{"", "https://admin.example.test", key, "OPENSITE_AUTH_DATABASE_URL or OPENSITE_AUTH_DB_PASSWORD"},
	} {
		set(tc.origin, tc.key, "")
		enabled, _, _, _, err := staffLoginEnvironment(tc.dsn)
		if enabled || err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-dsn") {
			t.Fatalf("incomplete identity configuration accepted or unclear: %v", err)
		}
	}
	set("https://admin.example.test", "not-hex", "")
	if enabled, _, _, _, err := staffLoginEnvironment("secret-dsn"); enabled || err == nil {
		t.Fatal("malformed signing key accepted")
	}
	set("https://admin.example.test", key, "192.0.2.0/24")
	enabled, origin, keyHex, proxies, err := staffLoginEnvironment("secret-dsn")
	if !enabled || err != nil || origin != "https://admin.example.test" || keyHex != key || proxies != "192.0.2.0/24" {
		t.Fatal("complete identity configuration not enabled")
	}
}
