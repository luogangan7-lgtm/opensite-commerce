// Responsibility: verify Fake webhook bytes with an isolated nonproduction key.
// Dependencies: stdlib HMAC/SHA256/JSON and payment contract; caller: HTTP adapter.
// No keys are logged/stored here. Only MAC-authenticated strict envelopes can
// become VerifiedEvent; checkout must still enforce durable database binding.
package checkout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type fakeHMACVerifier struct{ key []byte }

func NewFakeHMACVerifier(key []byte) (SignatureVerifier, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("Fake verifier requires at least32 bytes of isolated test key")
	}
	return &fakeHMACVerifier{key: bytes.Clone(key)}, nil
}
func (v *fakeHMACVerifier) Verify(ctx context.Context, raw []byte, signature string) (payment.VerifiedEvent, error) {
	if err := ctx.Err(); err != nil {
		return payment.VerifiedEvent{}, err
	}
	supplied, err := hex.DecodeString(signature)
	if err != nil || len(signature) != 64 || hex.EncodeToString(supplied) != signature {
		return payment.VerifiedEvent{}, unauthenticated()
	}
	mac := hmac.New(sha256.New, v.key)
	_, _ = mac.Write(raw)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return payment.VerifiedEvent{}, unauthenticated()
	}
	// This is a wire envelope, never VerifiedEvent decoding. Trust is established
	// by the MAC above; strict route admission separately rejects duplicate keys.
	var envelope httpapi.FakePaymentEvent
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&envelope) != nil {
		return payment.VerifiedEvent{}, unauthenticated()
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return payment.VerifiedEvent{}, unauthenticated()
	}
	event := payment.VerifiedEvent{EventID: envelope.EventId, TenantID: envelope.TenantId.String(), OrderID: envelope.OrderId.String(), OrderVersion: envelope.OrderVersion, ProviderRef: envelope.ProviderRef, Status: payment.PaymentStatus(envelope.Status), Amount: money.Money{Amount: envelope.AmountMinor, Currency: envelope.Currency}}
	if err := event.Validate(); err != nil {
		return payment.VerifiedEvent{}, unauthenticated()
	}
	return event, nil
}
