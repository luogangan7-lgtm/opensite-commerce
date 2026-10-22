// Responsibility: keep the retired tenant-authoritative Fake verifier fail closed.
// Dependencies: context, payment contract; callers: legacy source and replay guard.
// Invariant: no signature or envelope can regain tenant authority through this port.
package checkout

import (
	"context"
	"fmt"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

type retiredFakeVerifier struct{}

// NewFakeHMACVerifier retains source compatibility without an unsafe admission path.
// Deprecated: compose the trusted Payments receiver with isolated account bindings.
func NewFakeHMACVerifier(key []byte) (SignatureVerifier, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("Fake verifier requires at least 32 bytes of isolated test key")
	}
	return retiredFakeVerifier{}, nil
}

func (retiredFakeVerifier) Verify(ctx context.Context, _ []byte, _ string) (payment.VerifiedEvent, error) {
	if err := ctx.Err(); err != nil {
		return payment.VerifiedEvent{}, err
	}
	return payment.VerifiedEvent{}, unauthenticated()
}
