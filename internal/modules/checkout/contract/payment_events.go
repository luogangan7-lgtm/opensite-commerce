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

// PaymentEventApplier consumes facts after signature verification, trusted
// account mapping and inbox admission (V1_DECISIONS.md §2).
// The caller owns the transaction and rolls back an application error
// (C-19a, docs/architecture/modules/PAYMENTS_FAKE.md).
// A false result with nil error leaves the fact pending, such as a refund
// received before capture (issue #293).
type PaymentEventApplier interface {
	ApplyPaymentEvent(context.Context, pgx.Tx, payment.VerifiedEvent) (bool, error)
}
