//go:build pgx

// Responsibility: encode synthetic labels for DB fixtures after UUID admission.
// Dependencies: UUID and shared UUIDv5 helper; callers: tagged DB integration tests.
// Invariant: stable replay identities; preserve already canonical UUID inputs.
package db_test

import (
	"strings"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

func fixtureCallerKey(label string) string {
	if len(label) == 36 {
		if _, err := uuid.Parse(label); err == nil {
			return strings.ToLower(label)
		}
	}
	return idempotency.DerivedKey("db.fixture.v1", label)
}

func fixturePaymentEventKey(event string) string {
	return idempotency.DerivedKey("checkout.payment", event)
}
