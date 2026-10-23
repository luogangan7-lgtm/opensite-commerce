// Responsibility: transaction ports for the single Payments refund poster.
// Callers: Checkout and Returns; dependencies: Ledger value and caller's pgx tx.
// Invariants: provider confirmation precedes posting; the caller commits its receipt atomically.
package contract

import (
	"context"
	"github.com/jackc/pgx/v5"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
)

type RefundPoster interface {
	PostRefund(context.Context, pgx.Tx, ledger.Transaction) (string, error)
	LockReturnCheckout(context.Context, pgx.Tx, string) error
	PostReturnRefund(context.Context, pgx.Tx, string, string, string, ledger.Transaction) (string, error)
}
type RefundContext struct {
	ProviderRef    string            `json:"provider_ref"`
	PaymentVersion int64             `json:"payment_version"`
	AmountMinor    int64             `json:"amount_minor"`
	Currency       string            `json:"currency"`
	Accounts       map[string]string `json:"accounts"`
}
