// Responsibility: envelope validation, escaping and deterministic dedup gate.
// Dependencies: public contract/stdlib; verifies frozen message rendering.
package service

import (
	"strings"
	"testing"
)

func TestRenderEscapesAndStableKey(t *testing.T) {
	m, err := Render("11111111-1111-1111-1111-111111111111", "buyer@safe.test", "zh", "order.paid", "<script>alert(1)</script>")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Fatal("NOTIFICATION_HTML_ESCAPE", m.HTML)
	}
	n, _ := Render("11111111-1111-1111-1111-111111111111", "buyer@safe.test", "en", "order.paid", "different")
	if m.Key != n.Key {
		t.Fatal("NOTIFICATION_STABLE_KEY")
	}
}
func TestRenderRejectsUnsafeEnvelope(t *testing.T) {
	for _, address := range []string{"no-at-sign", "buyer@safe.test\r\nBcc:x@safe.test", "Name <buyer@safe.test>"} {
		if _, e := Render("event", address, "en", "order.paid", "ref"); e == nil {
			t.Fatalf("NOTIFICATION_UNSAFE_RECIPIENT %q", address)
		}
	}
	if _, e := Render("event", "a@safe.test", "en", "unknown", "ref"); e == nil {
		t.Fatal("NOTIFICATION_UNKNOWN_TEMPLATE")
	}
}
