// Responsibility: pin Fake-only checkout startup isolation without opening DB.
// Dependencies: environment parser; no runtime IO or credentials.
// Invariant: production cannot opt into deterministic payment even with keys.
package bootstrap

import "testing"

func TestCheckoutEnvironmentRejectsProductionFake(t *testing.T) {
	t.Setenv("OPENSITE_FAKE_PSP", "1")
	t.Setenv("OPENSITE_ENVIRONMENT", "production")
	if _, err := checkoutEnvironment("postgresql://unused.invalid/db"); err == nil {
		t.Fatal("production Fake mode admitted")
	}
}

func TestCheckoutEnvironmentDisabledWithoutFakeFlag(t *testing.T) {
	t.Setenv("OPENSITE_FAKE_PSP", "0")
	settings, err := checkoutEnvironment("postgresql://unused.invalid/db")
	if err != nil || settings != nil {
		t.Fatalf("disabled Fake mode mounted checkout: settings=%v err=%v", settings, err)
	}
}
