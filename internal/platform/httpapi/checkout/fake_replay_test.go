// Responsibility: reject replayable raw Fake signatures lacking a signed timestamp.
// Dependencies: retired Fake verifier; called by CI's default suite.
// Invariant: a correct MAC alone is insufficient webhook admission.
package checkout_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"testing"
)

func TestFakeWebhookRejectsTimestampFreeMAC(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	verifier, err := checkout.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	// Preserve the formerly accepted tenant-authoritative envelope exactly.
	legacy := `{"event_id":"evt-fixture","tenant_id":"` + tenant + `","order_id":"` + orderID + `","order_version":1,"provider_ref":"fake-fixture","status":"captured","amount_minor":1200,"currency":"USD"}`
	_, _ = mac.Write([]byte(legacy))
	if _, err := verifier.Verify(context.Background(), []byte(legacy), hex.EncodeToString(mac.Sum(nil))); err == nil {
		t.Fatal("timestamp-free authenticated envelope accepted: unbounded webhook replay")
	}
}

func TestRetiredFakeVerifierRejectsTimestampedTenantEnvelope(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	verifier, err := checkout.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"event_id":"evt-fixture","tenant_id":"` + tenant + `","order_id":"` + orderID + `","order_version":1,"provider_ref":"fake-fixture","status":"captured","amount_minor":1200,"currency":"USD"}`
	timestamp := "1791900000"
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(timestamp + "." + legacy))
	if _, err := verifier.Verify(context.Background(), []byte(legacy), "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil))); err == nil {
		t.Fatal("retired verifier regained tenant authority through timestamped signature")
	}
}
