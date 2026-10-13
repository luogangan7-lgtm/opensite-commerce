// Responsibility: pin explicit dev/test provider policy and actor provenance.
// Dependencies: bootstrap private parser and public actor contracts.
// Invariant: no implicit Fake, production Fake, or URL-only quote capability.
package bootstrap

import (
	"context"
	"encoding/base64"
	"github.com/jackc/pgx/v5"
	cc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"testing"
)

func TestQuoteCheckoutEnvironmentRequiresExplicitDevelopmentCapability(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, c := range []struct {
		name, environment, enabled, key string
		want, fail                      bool
	}{
		{"default-deny", "", "", "", false, false},
		{"production-disabled", "production", "false", "", false, false},
		{"implicit-environment", "", "true", key, false, true},
		{"production-fake", "production", "true", key, false, true},
		{"partial-secret", "test", "false", key, false, true},
		{"partial-enabled", "test", "true", "", false, true},
		{"noncanonical-flag", "test", "1", key, false, true},
		{"bad-key", "test", "true", "invalid", false, true},
		{"development", "development", "true", key, true, false},
		{"test", "test", "true", key, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			values := map[string]string{"OPENSITE_ENVIRONMENT": c.environment, "OPENSITE_CHECKOUT_FAKE_ENABLED": c.enabled, "OPENSITE_CHECKOUT_FAKE_SIGNING_KEY": c.key}
			out, err := checkoutEnvironment(func(k string) string { return values[k] })
			if (err != nil) != c.fail || out.enabled != c.want {
				t.Fatalf("enabled=%v error=%v", out.enabled, err)
			}
		})
	}
	if _, err := composeFakeCheckout(Config{}, runtimeQuoteRunner{}, fakeCheckoutEnvironment{enabled: true, key: make([]byte, 32), environment: "test"}); err == nil {
		t.Fatal("missing persistence/customer capability accepted")
	}
	t.Setenv("OPENSITE_ENVIRONMENT", "production")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_ENABLED", "true")
	t.Setenv("OPENSITE_CHECKOUT_FAKE_SIGNING_KEY", key)
	if _, close, err := FromEnvironment(context.Background(), Config{}); err == nil {
		if close != nil {
			close()
		}
		t.Fatal("production environment composed Fake")
	}
}

func TestQuoteRuntimeAdmissionRequiresVerifiedActor(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const buyer = "22222222-2222-4222-8222-222222222222"
	const company = "33333333-3333-4333-8333-333333333333"
	writes, snapshots := 0, 0
	r := runtimeQuoteRunner{run: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		writes++
		return fn(ctx, nil)
	}, snapshot: func(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
		snapshots++
		return fn(ctx, nil)
	}}
	fn := func(context.Context, pgx.Tx) error { return nil }
	if err := r.InTenant(context.Background(), tenant, fn); err == nil {
		t.Fatal("URL tenant alone accepted")
	}
	verifiedCustomer := cc.WithActor(context.Background(), cc.Actor{TenantID: tenant, CustomerID: buyer})
	if err := r.InTenant(verifiedCustomer, tenant, fn); err != nil || writes != 0 || snapshots != 1 {
		t.Fatalf("customer must use snapshot: %v", err)
	}
	admittedBuyer := qc.WithTrustedActor(verifiedCustomer, qc.ActorIdentity{TenantID: tenant, CustomerID: buyer, CompanyID: company})
	if err := r.InTenant(admittedBuyer, tenant, fn); err != nil || writes != 1 {
		t.Fatalf("admitted buyer: %v", err)
	}
	if err := r.InTenant(admittedBuyer, buyer, fn); err == nil {
		t.Fatal("actor tenant mismatch accepted")
	}
}
