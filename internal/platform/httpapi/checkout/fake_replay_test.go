// Responsibility: reject replayable raw Fake signatures lacking a signed timestamp.
// Dependencies: the existing Fake verifier; called by CI's default suite.
// Invariant: a correct MAC alone is insufficient webhook admission.
package checkout_test

import (
 "context"
 "crypto/hmac"
 "crypto/rand"
 "crypto/sha256"
 "encoding/hex"
 "testing"
 checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

func TestFakeWebhookRejectsTimestampFreeMAC(t *testing.T) {
 key := make([]byte, 32)
 if _, err := rand.Read(key); err != nil { t.Fatal(err) }
 verifier, err := checkout.NewFakeHMACVerifier(key)
 if err != nil { t.Fatal(err) }
 mac := hmac.New(sha256.New, key)
 _, _ = mac.Write([]byte(eventBody))
 if _, err := verifier.Verify(context.Background(), []byte(eventBody), hex.EncodeToString(mac.Sum(nil))); err == nil {
  t.Fatal("timestamp-free authenticated envelope accepted: unbounded webhook replay")
 }
}
