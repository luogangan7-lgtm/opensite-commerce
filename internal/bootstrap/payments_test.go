// Responsibility: guard the explicit Fake PSP/environment/key capability before IO.
// Dependencies: bootstrap environment parser and stdlib; caller: CI default suite.
// Invariant: malformed toggles, production modes and invalid isolated keys fail closed.
package bootstrap

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestFakePaymentEnvironmentAdmission(t *testing.T) {
	key := func(n int) string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, n)) }
	for _, tc := range []struct {
		name, flag, environment, key string
		enabled, rejected            bool
	}{
		{"unset", "", "production", "", false, false},
		{"off", "0", "production", "invalid", false, false},
		{"invalid toggle", "true", "test", key(32), false, true},
		{"empty environment", "1", "", key(32), false, true},
		{"production", "1", "production", key(32), false, true},
		{"case variant", "1", "Test", key(32), false, true},
		{"staging", "1", "staging", key(32), false, true},
		{"missing key", "1", "test", "", false, true},
		{"bad encoding", "1", "test", "!", false, true},
		{"short key", "1", "test", key(31), false, true},
		{"long key", "1", "development", key(129), false, true},
		{"test", "1", "test", key(32), true, false},
		{"development", "1", "development", key(128), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENSITE_FAKE_PSP", tc.flag)
			t.Setenv("OPENSITE_ENVIRONMENT", tc.environment)
			t.Setenv("OPENSITE_FAKE_WEBHOOK_KEY", tc.key)
			enabled, got, err := fakePaymentEnvironment()
			if (err != nil) != tc.rejected || enabled != tc.enabled {
				t.Fatalf("enabled=%v err=%v", enabled, err)
			}
			if tc.enabled {
				expected, decodeErr := base64.RawURLEncoding.Strict().DecodeString(tc.key)
				if decodeErr != nil || !bytes.Equal(got, expected) {
					t.Fatal("isolated key changed during admission")
				}
			} else if len(got) != 0 {
				t.Fatal("disabled/rejected composition returned key material")
			}
		})
	}
}

func TestStripeSandboxSelection(t *testing.T) {
	for _, tc := range []struct {
		provider, environment, key string
		enabled, reject            bool
	}{
		{"", "production", "", false, false}, {"fake", "production", "", false, false},
		{"stripe", "test", "sk_test_fixture", false, true}, {"stripe-sandbox", "production", "sk_test_fixture", false, true},
		{"stripe-sandbox", "test", "sk_live_fixture", false, true}, {"stripe-sandbox", "test", "rk_live_fixture", false, true},
		{"stripe-sandbox", "test", "sk_test_fixture", true, false},
	} {
		t.Run(tc.provider+tc.environment, func(t *testing.T) {
			t.Setenv("OPENSITE_PAYMENT_PROVIDER", tc.provider)
			t.Setenv("OPENSITE_ENVIRONMENT", tc.environment)
			t.Setenv("STRIPE_TEST_SECRET_KEY", tc.key)
			enabled, err := stripeSandboxEnvironment()
			if enabled != tc.enabled || (err != nil) != tc.reject {
				t.Fatal("provider selection admission drift")
			}
		})
	}
}
