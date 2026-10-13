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

// PrepaymentPort is the future checkout adapter for an already-created immutable
// order. It must persist intent/capture/cancel claims, validate owned live holds
// BEFORE Capture, and post paid + balanced ledger + outbox atomically. Repeated
// calls adopt the same order, never create a second intent or repeat a capture.
// No implementation is wired to production until checkout's quote adapter lands.
type PrepaymentPort interface {
	Prepay(context.Context, AuthorizedActor, Conversion) (Prepayment, error)
}

type Prepayment struct {
	OrderID, ProviderRef, State string
	Amount                      money.Money
}
