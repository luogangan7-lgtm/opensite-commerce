// Package contract defines orders' public commands and the read-only payable
// port. Checkout and verified payment consumers depend on this package; orders
// never imports a payment implementation. All commands use a caller-owned,
// authorized tenant transaction and preserve immutable integer-money snapshots.
package contract

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

var (
	ErrInvalid  = errors.New("orders: invalid input")
	ErrNotFound = errors.New("orders: not found")
	ErrConflict = errors.New("orders: state conflict")
	// ErrRetry: the database aborted the transaction (40001/40P01). Roll back
	// and retry the whole transaction; it is not a business-state conflict.
	ErrRetry            = errors.New("orders: transaction aborted, retry")
	ErrKeyReused        = errors.New("orders: idempotency key reused")
	ErrReservationOwner = errors.New("orders: reservation owner mismatch")
	ErrStockUnavailable = errors.New("orders: stock unavailable")
	ErrUnavailable      = errors.New("orders: unavailable")
	// ErrReservationInactive: a hold is released, expired or within the
	// requested remaining lifetime; the order can no longer be paid safely.
	ErrReservationInactive = errors.New("orders: reservation inactive")
)

type State string

const (
	PendingPayment     State = "pending_payment"
	Paid               State = "paid"
	PartiallyFulfilled State = "partially_fulfilled"
	Fulfilled          State = "fulfilled"
	Cancelled          State = "cancelled"
	PartiallyRefunded  State = "partially_refunded"
	Refunded           State = "refunded"
)

// Contact is a frozen buyer contact, never a lookup of a mutable profile.
// Customer buyers use the tenant-scoped customer FK. Authorization to select a
// buyer still belongs to the trusted caller's transaction boundary.
type Contact struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Phone string `json:"phone,omitempty"`
}
type Buyer struct {
	CustomerID string
	Guest      Contact
}

// QuoteSource identifies an already approved immutable negotiated snapshot.
// The trusted conversion caller must bind the quote to this order in the same
// transaction; constructing this value does not establish quote authorization.
type QuoteSource struct {
	QuoteID string `json:"quote_id"`
	Version int64  `json:"version"`
	LineNo  int32  `json:"line_no"`
}
type Line struct {
	ID, SKUID, SKU string
	Quantity       int64
	Price          pricing.Result
	Tax            tax.LineResult
	// QuoteSource is accepted only by CreateFromQuote; price-list provenance
	// must be absent. Monetary and tax bindings are identical to ordinary Create.
	QuoteSource *QuoteSource
	// An existing reservation is accepted only for this exact order, SKU and qty.
	// Empty requests a new order-owned reservation in the same transaction.
	ReservationID string
}
type Adjustment struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	AmountMinor int64  `json:"amount_minor"`
	Reason      string `json:"reason"`
}
type CreateInput struct {
	// IDs must be stable across retries; callers assign them before the command.
	ID, IdempotencyKey    string
	Buyer                 Buyer
	Currency              string
	Lines                 []Line
	Shipping              shipping.Result
	Adjustments           []Adjustment
	ReservationTTLSeconds int32
}
type Order struct {
	ID, TenantID    string
	Number, Version int64
	State           State
	Total           money.Money
}
type TransitionInput struct {
	OrderID string
	Target  State
	// EventKey identifies a trusted event/command, not an unverified HTTP claim.
	EventKey    string
	RefundMinor int64
}
type Payable struct {
	OrderID                                string
	CustomerID                             string
	Amount                                 money.Money
	IntentKey                              string
	Version                                int64
	ItemsNetMinor, TaxMinor, ShippingMinor int64
}
type PayableReader interface {
	GetPayable(context.Context, pgx.Tx, string, string) (Payable, error)
}
type Service interface {
	PayableReader
	Create(context.Context, pgx.Tx, string, CreateInput) (Order, error)
	Transition(context.Context, pgx.Tx, string, TransitionInput) (Order, error)
	// LockLiveReservations locks the parent order before every sorted hold for the
	// caller transaction and fails with ErrReservationInactive unless each is active
	// with at least minRemainingSeconds left. Payment capture calls it first.
	LockLiveReservations(ctx context.Context, tx pgx.Tx, tenantID, orderID string, minRemainingSeconds int32) error
}

// QuoteCreator extends negotiated checkout without weakening the price-list
// Create contract or forcing unrelated Service consumers to implement it.
type QuoteCreator interface {
	CreateFromQuote(context.Context, pgx.Tx, string, CreateInput) (Order, error)
}
