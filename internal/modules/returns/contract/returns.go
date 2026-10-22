// Package contract owns return commands and trusted refund composition ports.
// Dependencies: pgx, money, Payment Port and Ledger contract; callers: bootstrap
// and authorized admin API. Invariants: no client provider/account/amount proof;
// durable preparation precedes PSP, and ledger/order/receipt finalize atomically.
package contract

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

var (
	ErrInvalid      = errors.New("invalid return input")
	ErrNotFound     = errors.New("return resource not found")
	ErrConflict     = errors.New("return state or quantity conflict")
	ErrPrecondition = errors.New("return order version changed")
	ErrKeyReused    = errors.New("return idempotency key reused")
	ErrRetry        = errors.New("return transaction retry required")
	ErrUnavailable  = errors.New("return service unavailable")
)

type Kind string

const (
	ShipmentReturn  Kind = "shipment_return"
	UnshippedCancel Kind = "unshipped_cancel"
)

type Line struct {
	OrderLineID    string `json:"order_line_id"`
	ShipmentLineID string `json:"shipment_line_id,omitempty"`
	Quantity       int64  `json:"quantity"`
}
type RequestInput struct {
	OrderID         string
	ExpectedVersion int64
	IdempotencyKey  string
	Kind            Kind
	Reason          string
	Lines           []Line
}
type ReceiveInput struct {
	ReturnID        string
	ExpectedVersion int64
	IdempotencyKey  string
	Restock         bool
}
type RefundInput struct {
	ReturnID        string
	ExpectedVersion int64
	IdempotencyKey  string
}
type Return struct {
	ID           string       `json:"id"`
	OrderID      string       `json:"order_id"`
	OrderVersion int64        `json:"order_version"`
	Status       string       `json:"status"`
	Kind         Kind         `json:"kind"`
	Lines        []ReturnLine `json:"lines"`
	Restocked    bool         `json:"restocked"`
	RefundMinor  int64        `json:"refund_minor"`
}
type ReturnLine struct {
	ID             string `json:"id"`
	OrderLineID    string `json:"order_line_id"`
	ShipmentLineID string `json:"shipment_line_id,omitempty"`
	Quantity       int64  `json:"quantity"`
}
type PreparedRefund struct {
	Return
	RefundKey             string `json:"refund_key"`
	RefundExpectedVersion int64  `json:"refund_expected_version"`
	ProviderRef           string `json:"provider_ref"`
	PaymentVersion        int64  `json:"payment_version"`
	Currency              string `json:"currency"`
	IncomeMinor           int64  `json:"income_minor"`
	TaxMinor              int64  `json:"tax_minor"`
	ShippingMinor         int64  `json:"shipping_minor"`
	IncomeAccountID       string `json:"income_account_id"`
	TaxAccountID          string `json:"tax_account_id"`
	ShippingAccountID     string `json:"shipping_account_id"`
	CashAccountID         string `json:"cash_account_id"`
	LedgerTransactionID   string `json:"ledger_transaction_id"`
}

// InTenant must enforce authorization before opening the callback transaction.
// Each invocation commits only when fn returns nil; transactions never escape.
type TransactionRunner interface {
	InTenant(context.Context, string, func(context.Context, pgx.Tx) error) error
}
type CapturedPayment struct {
	TenantID, OrderID, ProviderRef string
	OrderVersion                   int64
	Paid                           money.Money
}
type CapturedPaymentReader interface {
	CapturedPayment(context.Context, pgx.Tx, string, string) (CapturedPayment, error)
}
type RefundAccounts struct{ Income, Tax, Shipping, Cash string }
type AccountReader interface {
	RefundAccounts(context.Context, pgx.Tx, string, string) (RefundAccounts, error)
}
type LedgerPoster interface {
	Post(context.Context, pgx.Tx, ledger.Transaction) (string, error)
}
type Config struct {
	Runner   TransactionRunner
	Payments payment.Port
	Captures CapturedPaymentReader
	Accounts AccountReader
	Ledger   LedgerPoster
}
type Service interface {
	Request(context.Context, pgx.Tx, string, RequestInput) (Return, error)
	Receive(context.Context, pgx.Tx, string, ReceiveInput) (Return, error)
	Refund(context.Context, string, RefundInput) (Return, error)
}

// RefundCompleter atomically commits an optional caller receipt with the final
// ledger/order/return transaction. Replays also run complete in an authorized
// transaction; callback failure preserves the durable pending refund for retry.
type RefundCompleter interface {
	RefundWithCompletion(context.Context, string, RefundInput, func(context.Context, pgx.Tx, Return) error) (Return, error)
}

func ValidID(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id != uuid.Nil && id.String() == s
}
func ValidKey(s string) bool {
	return len(s) > 0 && len(s) <= 128 && !strings.ContainsFunc(s, func(c rune) bool { return c < '!' || c > '~' })
}
func (in RequestInput) Validate() error {
	if !ValidID(in.OrderID) || in.ExpectedVersion <= 0 || !ValidKey(in.IdempotencyKey) || len(in.Lines) < 1 || len(in.Lines) > 1000 || strings.TrimSpace(in.Reason) == "" || utf8.RuneCountInString(in.Reason) > 1000 || !utf8.ValidString(in.Reason) || strings.ContainsAny(in.Reason, "\x00\r\n") {
		return ErrInvalid
	}
	if in.Kind != ShipmentReturn && in.Kind != UnshippedCancel {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(in.Lines))
	for _, l := range in.Lines {
		if !ValidID(l.OrderLineID) || l.Quantity <= 0 {
			return ErrInvalid
		}
		if (in.Kind == ShipmentReturn && !ValidID(l.ShipmentLineID)) || (in.Kind == UnshippedCancel && l.ShipmentLineID != "") {
			return ErrInvalid
		}
		key := l.OrderLineID + "/" + l.ShipmentLineID
		if seen[key] {
			return ErrInvalid
		}
		seen[key] = true
	}
	return nil
}
func (in ReceiveInput) Validate() error {
	if !ValidID(in.ReturnID) || in.ExpectedVersion <= 0 || !ValidKey(in.IdempotencyKey) {
		return ErrInvalid
	}
	return nil
}
func (in RefundInput) Validate() error {
	if !ValidID(in.ReturnID) || in.ExpectedVersion <= 0 || !ValidKey(in.IdempotencyKey) {
		return ErrInvalid
	}
	return nil
}
