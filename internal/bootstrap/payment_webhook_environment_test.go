// Responsibility: refuse enabling test payment ingress in production before any IO.
// Dependencies: bootstrap environment composition; called by CI's default suite.
// Invariant: a Fake toggle is never silently ignored in production.
package bootstrap

import (
	"context"
	"os"
	"testing"
)

func TestPaymentWebhookProductionToggleFailsBeforeIO(t *testing.T) {
	for _, name := range []string{"isolated", "polluted_components"} {
		t.Run(name, func(t *testing.T) {
			for _, variable := range []string{
				"OPENSITE_DATABASE_URL", "OPENSITE_AUTH_DATABASE_URL", "OPENSITE_MEDIA_DATABASE_URL", "OPENSITE_DOMAIN_DATABASE_URL", "OPENSITE_CUSTOMER_AUTH_DATABASE_URL",
				"OPENSITE_RUNTIME_DB_PASSWORD", "OPENSITE_AUTH_DB_PASSWORD", "OPENSITE_MEDIA_DB_PASSWORD", "OPENSITE_DOMAIN_DB_PASSWORD", "OPENSITE_CUSTOMER_AUTH_DB_PASSWORD",
				"OSC_ADMIN_ORIGIN", "OSC_AUTH_SIGNING_KEY_HEX", "OSC_AUTH_TRUSTED_PROXIES", "OPENSITE_IDEMPOTENCY_ROOT", "OPENSITE_CMS_PREVIEW_KEY", "OPENSITE_MEDIA_ROOT",
			} {
				// Setenv registers restoration; LookupEnv must then see an absent key.
				t.Setenv(variable, "")
				if err := os.Unsetenv(variable); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("OPENSITE_ENVIRONMENT", "production")
			t.Setenv("OPENSITE_FAKE_PSP", "0")
			_, closeRuntime, err := FromEnvironment(context.Background(), Config{})
			if closeRuntime != nil {
				closeRuntime()
			}
			if err != nil {
				t.Fatalf("isolated no-Fake control failed: %v", err)
			}
			if name == "polluted_components" {
				t.Setenv("OPENSITE_AUTH_DB_PASSWORD", "invalid-test-component")
				t.Setenv("OSC_ADMIN_ORIGIN", "invalid-test-origin")
			}
			t.Setenv("OPENSITE_FAKE_PSP", "1")
			_, closeRuntime, err = FromEnvironment(context.Background(), Config{})
			if closeRuntime != nil {
				closeRuntime()
				t.Fatal("production Fake refusal initialized runtime cleanup")
			}
			if err == nil || err.Error() != "Fake payment is forbidden outside test/development" {
				t.Fatalf("expected pre-IO Fake refusal, got %v", err)
			}
		})
	}
}
