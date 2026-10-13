// Responsibility: quote-to-order receipts and the checkout adoption seam.
// Dependencies: money/pgx; callers: quotes conversion application and checkout adapter.
// Invariants: checkout owns provider IO, recovery and ledger; a receipt is not payment proof.
package contract

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

type Conversion struct {
	QuoteID, OrderID, CompanyID, CustomerID, IdempotencyKey string
	QuoteVersion, OrderNumber                               int64
	Amount                                                  money.Money
	State, ProviderRef                                      string
	CreatedAt                                               time.Time
}

// ConversionRepository commands borrow a tenant transaction. Lock checks current
// membership and approval/DB-clock expiry; Bind rechecks expiry after stock waits.
type ConversionRepository interface {
	LockForConversion(context.Context, pgx.Tx, AuthorizedActor, string) (Quote, error)
	BindConversion(context.Context, pgx.Tx, AuthorizedActor, string, string, int64, string, []byte) (Conversion, error)
	GetConversion(context.Context, pgx.Tx, AuthorizedActor, string) (Conversion, error)
	ConversionSKU(context.Context, pgx.Tx, AuthorizedActor, string, string) (string, error)
}

// PrepaymentPort is the checkout adoption adapter for an already-created immutable
// order. Prepay adopts the same durable intent on repeated calls. Checkout owns
// capture/cancel claims, validates owned live holds BEFORE Capture, and posts
// paid + balanced ledger + outbox atomically through its verified callbacks.
// Bootstrap derives its production implementation from ExistingOrderService;
// only verified provider callbacks can confirm payment through checkout.
type PrepaymentPort interface {
	Prepay(context.Context, AuthorizedActor, Conversion) (Prepayment, error)
}

type Prepayment struct {
	OrderID, ProviderRef, State string
	Amount                      money.Money
}
