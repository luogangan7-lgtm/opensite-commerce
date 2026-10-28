// Responsibility: stable UUIDs for internal operation identities, not caller admission.
// Dependencies: JSON tuple encoding and the existing UUIDv5 implementation.
// Callers: Orders, Checkout and Quotes derive namespaced copy/async keys.
// Invariant: namespace/tuple differences preserve replay isolation; HTTP caller
// keys still pass ValidateKey and direct caller writes still meet DB CHECKs.
package idempotency

import (
	"encoding/json"

	"github.com/google/uuid"
)

// DerivedKey uses a fixed per-purpose UUIDv5 namespace, derived from the OID
// namespace and the versioned purpose. JSON framing keeps input tuples distinct.
func DerivedKey(purpose string, parts ...string) string {
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(purpose))
	body, _ := json.Marshal(parts)
	return uuid.NewSHA1(namespace, body).String()
}
