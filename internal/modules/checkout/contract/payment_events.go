package contract

import (
	"context"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

type ServiceWithPaymentEvents interface {
	Service
	PaymentEventApplier
}

// PaymentEventApplier consumes verified facts within the caller's transaction.
// The caller verifies signature, account and inbox admission and owns rollback
// on error. A false result with nil error means a prerequisite is missing.
type PaymentEventApplier interface {
	ApplyPaymentEvent(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error)
}
