// Responsibility: expose transaction-scoped consumption of verified payment facts.
// Dependencies: neutral Payment v1 and pgx; caller: trusted Payments composition.
// Invariants: caller verifies signature/account/Inbox, owns commit and rolls back
// every error; false means a prerequisite is missing, never a provider command.
package contract

import (
	"context"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

type PaymentEventApplier interface {
	ApplyPaymentEvent(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error)
}
