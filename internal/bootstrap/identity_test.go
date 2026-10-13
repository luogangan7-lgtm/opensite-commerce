// Composition tests keep missing/partial credentials fail-closed before DB IO.
// Dependencies: only bootstrap; production auth authority is tested with real PG.
package bootstrap

import (
	"context"
	"strings"
	"testing"
)

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
