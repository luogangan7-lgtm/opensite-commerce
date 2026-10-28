// Responsibility: pin the internal Orders key namespace and raw-key boundary.
// Dependencies: pure quotes key functions and Go standard library; caller: Go CI.
// Invariants: bounded deterministic tuple encoding; receipts keep their raw key.
package quotes

import (
	"strings"
	"testing"
)

func TestConversionOrderKeyScope(t *testing.T) {
	const tenant = "00000000-0000-4000-8000-000000000001"
	const actor = "00000000-0000-4000-8000-000000000002"
	const quote = "00000000-0000-4000-8000-000000000003"
	const other = "00000000-0000-4000-8000-000000000004"
	base := conversionOrderKey(tenant, "customer", actor, quote, "once")
	// The literal is independent of the helper's serialization and operation.
	want := "8ef671bc-3eed-5550-b045-d4a8853f5df7"
	if base != want || len(base) != 36 || !conversionKey(base) {
		t.Fatalf("namespace key = %q, want %q (UUID)", base, want)
	}
	for _, tc := range []struct {
		name, tenant, kind, actor, quote, raw string
	}{
		{"tenant", other, "customer", actor, quote, "once"},
		{"actor-kind", tenant, "staff", actor, quote, "once"},
		{"actor-id", tenant, "customer", other, quote, "once"},
		{"quote", tenant, "customer", actor, other, "once"},
		{"raw-key", tenant, "customer", actor, quote, "twice"},
		{"raw-max", tenant, "customer", actor, quote, strings.Repeat("x", 128)},
		{"raw-json-escaping", tenant, "customer", actor, quote, `a\"b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := conversionOrderKey(tc.tenant, tc.kind, tc.actor, tc.quote, tc.raw)
			if got == base || len(got) != 36 || !conversionKey(got) || !conversionKey(tc.raw) {
				t.Fatalf("scope key=%q raw=%q", got, tc.raw)
			}
			if replay := conversionOrderKey(tc.tenant, tc.kind, tc.actor, tc.quote, tc.raw); replay != got {
				t.Fatalf("unstable key: %q then %q", got, replay)
			}
		})
	}
}

func TestConversionRawKeyValidation(t *testing.T) {
	// Convert validates the customer key before deriving any internal key.
	for _, raw := range []string{"", " ", "has space", "line\nfeed", "nul\x00byte", "nonasciié", strings.Repeat("x", 129)} {
		if conversionKey(raw) {
			t.Errorf("accepted invalid raw key %q", raw)
		}
	}
	if !conversionKey(strings.Repeat("x", 128)) {
		t.Fatal("128-byte raw key rejected")
	}
}
