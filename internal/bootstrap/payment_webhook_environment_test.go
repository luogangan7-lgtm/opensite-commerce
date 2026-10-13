// Responsibility: refuse enabling test payment ingress in production before any IO.
// Dependencies: bootstrap environment composition; called by CI's default suite.
// Invariant: a Fake toggle is never silently ignored in production.
package bootstrap

import (
	"context"
	"testing"
)

func TestPaymentWebhookProductionToggleFailsBeforeIO(t *testing.T) {
	for _, name := range []string{"OPENSITE_DATABASE_URL", "OPENSITE_AUTH_DATABASE_URL", "OPENSITE_MEDIA_DATABASE_URL", "OPENSITE_DOMAIN_DATABASE_URL", "OPENSITE_CUSTOMER_AUTH_DATABASE_URL", "OPENSITE_RUNTIME_DATABASE_PASSWORD", "OPENSITE_AUTH_DATABASE_PASSWORD", "OPENSITE_MEDIA_DATABASE_PASSWORD", "OPENSITE_DOMAIN_DATABASE_PASSWORD", "OPENSITE_CUSTOMER_AUTH_DATABASE_PASSWORD"} {
		t.Setenv(name, "")
	}
	t.Setenv("OPENSITE_ENVIRONMENT", "production")
	t.Setenv("OPENSITE_FAKE_PSP", "1")
	_, closeRuntime, err := FromEnvironment(context.Background(), Config{})
	if closeRuntime != nil {
		closeRuntime()
	}
	if err == nil {
		t.Fatal("production accepted explicit Fake payment ingress toggle")
	}
}
