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
	ErrInvalid          = errors.New("orders: invalid input")
	ErrNotFound         = errors.New("orders: not found")
	ErrConflict         = errors.New("orders: state conflict")
	ErrKeyReused        = errors.New("orders: idempotency key reused")
	ErrReservationOwner = errors.New("orders: reservation owner mismatch")
	ErrStockUnavailable = errors.New("orders: stock unavailable")
	ErrUnavailable      = errors.New("orders: unavailable")
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
// A customer UUID is accepted by the customer contract; its FK is an integration
// follow-up until the customers migration is available on the base branch.
type Contact struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Phone string `json:"phone,omitempty"`
}
type Buyer struct {
	CustomerID string
	Guest      Contact
}
type Line struct {
	ID, SKUID, SKU string
	Quantity       int64
	Price          pricing.Result
	Tax            tax.LineResult
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
	OrderID   string
	Amount    money.Money
	IntentKey string
	Version   int64
}
type PayableReader interface {
	GetPayable(context.Context, pgx.Tx, string, string) (Payable, error)
}
type Service interface {
	PayableReader
	Create(context.Context, pgx.Tx, string, CreateInput) (Order, error)
	Transition(context.Context, pgx.Tx, string, TransitionInput) (Order, error)
}
