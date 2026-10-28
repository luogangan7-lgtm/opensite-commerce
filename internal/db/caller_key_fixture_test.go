//go:build pgx

// Responsibility: encode synthetic labels for DB fixtures after UUID admission.
// Dependencies: UUID and SHA-256; callers: tagged DB integration tests.
// Invariant: stable replay identities; preserve already canonical UUID inputs.
package db_test

import (
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

func fixtureCallerKey(label string) string {
	if len(label) == 36 {
		if _, err := uuid.Parse(label); err == nil {
			return strings.ToLower(label)
		}
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceOID, []byte(label), 8).String()
}

func fixturePaymentEventKey(event string) string {
	body, _ := json.Marshal([]string{"checkout.payment", event})
	return uuid.NewHash(sha256.New(), uuid.NameSpaceOID, body, 8).String()
}
