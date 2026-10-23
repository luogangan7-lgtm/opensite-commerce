// Responsibility: sole refund journal posting and durable return/provider binding.
// Dependencies: Ledger port and generated closed SQL commands; callers: Checkout,
// Returns and bootstrap readers. Invariants: caller-owned tx, DB authority and locks;
// an identity conflict rolls back the journal, counter and RMA receipt together.
package payments

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
)

type RefundJournal struct{ Ledger ledger.Poster }

func (p RefundJournal) Post(ctx context.Context, tx pgx.Tx, in ledger.Transaction) (string, error) {
	return p.Ledger.Post(ctx, tx, in)
}
func (p RefundJournal) PostRefund(ctx context.Context, tx pgx.Tx, in ledger.Transaction) (string, error) {
	return p.Ledger.Post(ctx, tx, in)
}
func (p RefundJournal) LockReturnCheckout(ctx context.Context, tx pgx.Tx, returnID string) error {
	return db.New(tx).LockPaymentReturnCheckout(ctx, id(returnID))
}
func (p RefundJournal) PostReturnRefund(ctx context.Context, tx pgx.Tx, returnID, key, refundID string, in ledger.Transaction) (string, error) {
	if refundID == "" {
		return "", invalid()
	}
	j, err := db.New(tx).PostPaymentReturnRefund(ctx, db.PostPaymentReturnRefundParams{ReturnID: id(returnID), AttemptKey: key, ProviderRefundID: refundID})
	return j.String(), err
}
func (p RefundJournal) PaidContext(ctx context.Context, tx pgx.Tx, orderID string) (contract.RefundContext, error) {
	raw, err := db.New(tx).GetPaymentReturnContext(ctx, db.GetPaymentReturnContextParams{OrderID: id(orderID)})
	var out contract.RefundContext
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
func (m *Module) applyFact(ctx context.Context, tx pgx.Tx, event payment.VerifiedEvent, refundID, account string) (bool, error) {
	if event.Status == payment.StatusRefunded || event.Status == payment.StatusPartiallyRefunded {
		if refundID == "" {
			return false, &payment.PaymentError{Code: payment.ErrorStateConflict, Message: "refund requires stable provider identity"}
		}
		terminal, err := db.New(tx).IsPaymentRefundTerminal(ctx, db.IsPaymentRefundTerminalParams{Account: account, ProviderRefundID: refundID, OrderID: id(event.OrderID), ProviderRef: event.ProviderRef, PaymentVersion: event.OrderVersion, AmountMinor: event.Amount.Amount, Currency: event.Amount.Currency})
		if err != nil || terminal {
			return terminal, err
		}
		match, err := db.New(tx).MatchPaymentReturnRefund(ctx, db.MatchPaymentReturnRefundParams{OrderID: id(event.OrderID), ProviderRef: event.ProviderRef, ProviderRefundID: refundID, PaymentVersion: event.OrderVersion, AmountMinor: event.Amount.Amount, Currency: event.Amount.Currency})
		if err != nil {
			return false, err
		}
		switch match {
		case "matched":
			return true, nil
		case "pending":
			return false, nil
		case "conflict":
			return false, &payment.PaymentError{Code: payment.ErrorStateConflict, Message: "refund identity conflicts with return"}
		}
	}
	return m.config.Apply(ctx, tx, event)
}

func (p RefundJournal) RefundAccounts(ctx context.Context, tx pgx.Tx, currency string) (contract.RefundContext, error) {
	raw, err := db.New(tx).GetPaymentReturnContext(ctx, db.GetPaymentReturnContextParams{Currency: currency})
	var out contract.RefundContext
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
