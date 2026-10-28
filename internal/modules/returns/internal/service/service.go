// Package service owns return commands and durable refund orchestration.
// Dependencies: trusted capture/account/payment/ledger ports and private store.
// Callers: bootstrap/admin API. Invariants: PSP is outside database transactions;
// a prepared request survives failures, while final ledger/order/receipt commit
// together. Retry uses the persisted PSP fingerprint, never current order data.
// Finalization locks the order and return before any ledger or receipt write.
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	payments "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

type Repository interface {
	Request(context.Context, pgx.Tx, string, contract.RequestInput) (contract.Return, error)
	Receive(context.Context, pgx.Tx, string, contract.ReceiveInput) (contract.Return, error)
	OrderID(context.Context, pgx.Tx, string, string) (string, error)
	OrderTotal(context.Context, pgx.Tx, string, string) (contract.CapturedPayment, error)
	Prepared(context.Context, pgx.Tx, string, contract.RefundInput) (contract.PreparedRefund, bool, error)
	LockPrepared(context.Context, pgx.Tx, string, contract.RefundInput) (contract.PreparedRefund, bool, error)
	Prepare(context.Context, pgx.Tx, string, contract.RefundInput, contract.CapturedPayment, contract.RefundAccounts) (contract.PreparedRefund, error)
	Fail(context.Context, pgx.Tx, string, contract.PreparedRefund, bool) error
	Complete(context.Context, pgx.Tx, string, contract.PreparedRefund, string) (contract.Return, error)
}
type Service struct {
	config contract.Config
	repo   Repository
}

func (s *Service) Request(ctx context.Context, tx pgx.Tx, tenant string, in contract.RequestInput) (contract.Return, error) {
	if err := in.Validate(); err != nil {
		return contract.Return{}, err
	}
	return s.repo.Request(ctx, tx, tenant, in)
}
func (s *Service) Receive(ctx context.Context, tx pgx.Tx, tenant string, in contract.ReceiveInput) (contract.Return, error) {
	if err := in.Validate(); err != nil {
		return contract.Return{}, err
	}
	return s.repo.Receive(ctx, tx, tenant, in)
}
func (s *Service) Refund(ctx context.Context, tenant string, in contract.RefundInput) (contract.Return, error) {
	return s.RefundWithCompletion(ctx, tenant, in, nil)
}

func (s *Service) RefundWithCompletion(ctx context.Context, tenant string, in contract.RefundInput, complete func(context.Context, pgx.Tx, contract.Return) error) (contract.Return, error) {
	if err := in.Validate(); err != nil {
		return contract.Return{}, err
	}
	if !contract.ValidID(tenant) {
		return contract.Return{}, contract.ErrInvalid
	}
	if s == nil || s.repo == nil || s.config.Runner == nil {
		return contract.Return{}, contract.ErrUnavailable
	}
	var prepared contract.PreparedRefund
	err := s.config.Runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		var err error
		prepared, exists, err = s.repo.Prepared(ctx, tx, tenant, in)
		if err != nil {
			return err
		}
		// The durable intent is authoritative on retries, even when the original
		// capture/account configuration is unavailable or has since changed.
		if exists {
			if err := validatePrepared(tenant, in, prepared); err != nil {
				return err
			}
			if prepared.Status == "refunded" && complete != nil {
				return complete(ctx, tx, prepared.Return)
			}
			return nil
		}
		if s.config.Captures == nil || s.config.Accounts == nil {
			return contract.ErrUnavailable
		}
		orderID, err := s.repo.OrderID(ctx, tx, tenant, in.ReturnID)
		if err != nil {
			return err
		}
		total, err := s.repo.OrderTotal(ctx, tx, tenant, orderID)
		if err != nil {
			return err
		}
		capture, err := s.config.Captures.CapturedPayment(ctx, tx, tenant, orderID)
		if err != nil {
			return err
		}
		if !validCapture(capture, total, tenant, orderID) {
			return contract.ErrUnavailable
		}
		accounts, err := s.config.Accounts.RefundAccounts(ctx, tx, tenant, capture.Paid.Currency)
		if err != nil {
			return err
		}
		if !validAccounts(accounts) {
			return contract.ErrUnavailable
		}
		prepared, err = s.repo.Prepare(ctx, tx, tenant, in, capture, accounts)
		if err != nil {
			return err
		}
		if err = validatePrepared(tenant, in, prepared); err != nil {
			return err
		}
		if prepared.OrderID != orderID || prepared.ProviderRef != capture.ProviderRef || prepared.PaymentVersion != capture.OrderVersion || prepared.Currency != capture.Paid.Currency || prepared.RefundMinor > capture.Paid.Amount {
			return contract.ErrUnavailable
		}
		if prepared.Status == "refunded" && complete != nil {
			return complete(ctx, tx, prepared.Return)
		}
		return nil
	})
	if err != nil {
		return contract.Return{}, err
	}
	if prepared.Status == "refunded" {
		return prepared.Return, nil
	}
	if s.config.Payments == nil || s.config.Ledger == nil {
		return contract.Return{}, contract.ErrUnavailable
	}
	poster, ok := s.config.Ledger.(payments.RefundPoster)
	if !ok {
		return contract.Return{}, contract.ErrUnavailable
	}
	request := payment.PaymentOperation{CreatePaymentIntent: payment.CreatePaymentIntent{TenantID: tenant, OrderID: prepared.OrderID, OrderVersion: prepared.PaymentVersion, Amount: money.Money{Amount: prepared.RefundMinor, Currency: prepared.Currency}, IdempotencyKey: idempotency.DerivedKey("return.provider.v1", prepared.ID, prepared.RefundKey)}, ProviderRef: prepared.ProviderRef}
	result, err := s.config.Payments.Refund(ctx, request)
	if err != nil {
		return contract.Return{}, s.recordProviderFailure(ctx, tenant, prepared, err)
	}
	if result.RefundID == "" || result.ProviderRef != prepared.ProviderRef || (result.Status != payment.StatusPartiallyRefunded && result.Status != payment.StatusRefunded) {
		return contract.Return{}, s.recordProviderFailure(ctx, tenant, prepared, contract.ErrUnavailable)
	}
	var out contract.Return
	err = s.config.Runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := poster.LockReturnCheckout(ctx, tx, prepared.ID); err != nil {
			return store.MapError(err)
		}
		latest, exists, err := s.repo.LockPrepared(ctx, tx, tenant, in)
		if err != nil {
			return err
		}
		if !exists || validatePrepared(tenant, in, latest) != nil || !sameIntent(prepared, latest) {
			return contract.ErrUnavailable
		}
		if latest.Status == "refunded" {
			out = latest.Return
			if complete != nil {
				return complete(ctx, tx, out)
			}
			return nil
		}
		posting, err := refundJournal(tenant, prepared)
		if err != nil {
			return err
		}
		journal, err := poster.PostReturnRefund(ctx, tx, prepared.ID, prepared.RefundKey, result.RefundID, posting)
		if err != nil {
			return store.MapError(err)
		}
		if !contract.ValidID(journal) {
			return contract.ErrUnavailable
		}
		out, err = s.repo.Complete(ctx, tx, tenant, prepared, journal)
		if err != nil {
			return err
		}
		if complete != nil {
			return complete(ctx, tx, out)
		}
		return nil
	})
	if err != nil {
		return contract.Return{}, err
	}
	return out, nil
}

// Only an adapter's permanently sealed no-effect proof may release a reservation.
// Timeouts, unknown results and unmarked errors need reconciliation with the
// same frozen PSP key. Persist even if the caller disconnected, but bound I/O.
func (s *Service) recordProviderFailure(ctx context.Context, tenant string, prepared contract.PreparedRefund, cause error) error {
	var providerError *payment.PaymentError
	rejected := errors.As(cause, &providerError) && providerError.NoSideEffect && !providerError.Retryable()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.config.Runner.InTenant(persistCtx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.Fail(ctx, tx, tenant, prepared, rejected)
	}); err != nil {
		return err
	}
	return cause
}

func validCapture(capture, total contract.CapturedPayment, tenant, orderID string) bool {
	return capture.TenantID == tenant && capture.OrderID == orderID && capture.OrderVersion > 0 && strings.TrimSpace(capture.ProviderRef) != "" && len(capture.ProviderRef) <= 256 && !strings.ContainsAny(capture.ProviderRef, "\x00\r\n") && capture.Paid.Amount > 0 && capture.Paid.Validate() == nil && total.TenantID == tenant && total.OrderID == orderID && capture.Paid == total.Paid
}
func validAccounts(a contract.RefundAccounts) bool {
	seen := make(map[string]bool, 4)
	for _, id := range []string{a.Income, a.Tax, a.Shipping, a.Cash} {
		if !contract.ValidID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func validatePrepared(tenant string, in contract.RefundInput, p contract.PreparedRefund) error {
	if p.RefundExpectedVersion != in.ExpectedVersion {
		return contract.ErrKeyReused
	}
	if p.ID != in.ReturnID || !contract.ValidID(p.OrderID) || p.OrderVersion <= 0 || p.RefundKey != in.IdempotencyKey || p.RefundMinor <= 0 || p.PaymentVersion <= 0 || strings.TrimSpace(p.ProviderRef) == "" || !validAccounts(contract.RefundAccounts{Income: p.IncomeAccountID, Tax: p.TaxAccountID, Shipping: p.ShippingAccountID, Cash: p.CashAccountID}) {
		return contract.ErrUnavailable
	}
	if p.Status != "refund_pending" && p.Status != "refund_reconcile" && p.Status != "refunded" {
		return contract.ErrConflict
	}
	if p.Status == "refunded" && !contract.ValidID(p.LedgerTransactionID) {
		return contract.ErrUnavailable
	}
	_, err := refundJournal(tenant, p)
	return err
}
func sameIntent(a, b contract.PreparedRefund) bool {
	return a.ID == b.ID && a.OrderID == b.OrderID && a.RefundKey == b.RefundKey && a.RefundExpectedVersion == b.RefundExpectedVersion && a.ProviderRef == b.ProviderRef && a.PaymentVersion == b.PaymentVersion && a.Currency == b.Currency && a.RefundMinor == b.RefundMinor && a.IncomeMinor == b.IncomeMinor && a.TaxMinor == b.TaxMinor && a.ShippingMinor == b.ShippingMinor && a.IncomeAccountID == b.IncomeAccountID && a.TaxAccountID == b.TaxAccountID && a.ShippingAccountID == b.ShippingAccountID && a.CashAccountID == b.CashAccountID
}
func refundJournal(tenant string, p contract.PreparedRefund) (ledger.Transaction, error) {
	posting := ledger.Transaction{TenantID: tenant, Currency: p.Currency, SourceType: "return", SourceID: p.ID, Event: "refund"}
	for _, part := range []struct {
		account string
		amount  int64
	}{{p.IncomeAccountID, p.IncomeMinor}, {p.TaxAccountID, p.TaxMinor}, {p.ShippingAccountID, p.ShippingMinor}} {
		if part.amount < 0 {
			return ledger.Transaction{}, contract.ErrUnavailable
		}
		if part.amount > 0 {
			posting.Entries = append(posting.Entries, ledger.Entry{AccountID: part.account, Side: ledger.Debit, Amount: money.Money{Amount: part.amount, Currency: p.Currency}})
		}
	}
	posting.Entries = append(posting.Entries, ledger.Entry{AccountID: p.CashAccountID, Side: ledger.Credit, Amount: money.Money{Amount: p.RefundMinor, Currency: p.Currency}})
	if posting.Validate() != nil {
		return ledger.Transaction{}, contract.ErrUnavailable
	}
	return posting, nil
}
