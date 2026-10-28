// Responsibility: stable UUIDs for internal operation identities, not caller admission.
// Dependencies: JSON tuple encoding, SHA-256 and the existing UUID package.
// Callers: Orders, Checkout and Quotes derive namespaced copy/async keys.
// Invariant: namespace/tuple differences preserve replay isolation; HTTP caller
// keys still pass ValidateKey and direct caller writes still meet DB CHECKs.
package idempotency

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/google/uuid"
)

func DerivedKey(parts ...string) string {
	body, _ := json.Marshal(parts)
	return uuid.NewHash(sha256.New(), uuid.NameSpaceOID, body, 8).String()
}
