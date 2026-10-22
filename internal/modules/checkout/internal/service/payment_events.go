// Responsibility: consume completed payment facts without invoking the provider.
// Dependencies: checkout store/receipts, Orders and Ledger public transaction ports.
// Caller: Payments through the public applier; it owns transaction and Inbox result.
// Invariants: checkout lock serializes commands/facts; original payable stays fixed;
// refund components are bounded, and order/journal/receipt commit atomically.
package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

var _ contract.PaymentEventApplier = (*Service)(nil)

func (s *Service) ApplyPaymentEvent(ctx context.Context, tx pgx.Tx, event payment.VerifiedEvent) (bool, error) {
	applied, err := s.applyPaymentFact(ctx, tx, event)
	// A caller may quarantine these facts after rolling its savepoint back. Do
	// not classify unknown/availability/serialization failures as poison events.
	for _, conflict := range []error{contract.ErrConflict, contract.ErrInvalid, contract.ErrNotFound, contract.ErrKeyReused,
		orders.ErrConflict, orders.ErrInvalid, orders.ErrNotFound, orders.ErrKeyReused,
		orders.ErrReservationInactive, orders.ErrReservationOwner, orders.ErrStockUnavailable} {
		if errors.Is(err, conflict) {
			return false, &payment.PaymentError{Code: payment.ErrorStateConflict, Message: "payment fact conflicts with persisted order"}
		}
	}
	return applied, err
}

func (s *Service) applyPaymentFact(ctx context.Context, tx pgx.Tx, event payment.VerifiedEvent) (bool, error) {
	if event.Validate() != nil {
		return false, contract.ErrInvalid
	}
	refund := event.Status == payment.StatusPartiallyRefunded || event.Status == payment.StatusRefunded
	if !refund && event.Status != payment.StatusCaptured && event.Status != payment.StatusFailed && event.Status != payment.StatusCanceled {
		return false, contract.ErrInvalid
	}
	if s == nil || s.config.Orders == nil || s.config.Ledger == nil {
		return false, contract.ErrUnavailable
	}
	q, err := store.Queries(ctx, tx, event.TenantID)
	if err != nil {
		return false, err
	}
	row, err := q.GetCheckoutByOrder(ctx, db.GetCheckoutByOrderParams{TenantID: store.UUID(event.TenantID), OrderID: store.UUID(event.OrderID)})
	if err != nil {
		return false, store.MapError(err)
	}
	if row.ProviderRef != event.ProviderRef || row.PayableVersion != event.OrderVersion || row.Currency != event.Amount.Currency ||
		(!refund && row.AmountMinor != event.Amount.Amount) || (refund && event.Amount.Amount > row.AmountMinor) {
		return false, contract.ErrConflict
	}
	// The same permanent identity used by command confirmation binds all facts.
	hash := digest(event)
	attempt, err := q.GetCheckoutAttempt(ctx, db.GetCheckoutAttemptParams{TenantID: row.TenantID, EventKind: "payment", EventKey: event.EventID})
	if err == nil {
		if attempt.CheckoutID != row.ID || !bytes.Equal(attempt.RequestHash, hash) {
			return false, contract.ErrKeyReused
		}
		if attempt.Result != nil {
			return true, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, store.MapError(err)
	} else if _, err = q.InsertCheckoutAttempt(ctx, db.InsertCheckoutAttemptParams{TenantID: row.TenantID, EventKind: "payment", EventKey: event.EventID, CheckoutID: row.ID, RequestHash: hash}); err != nil {
		return false, store.MapError(err)
	}
	if refund {
		if row.Phase == "ready" || row.Phase == "confirming" {
			return false, nil
		}
		if row.Phase != "paid" {
			return false, contract.ErrConflict
		}
		err = s.applyRefundFact(ctx, tx, q, row, event)
	} else {
		err = s.applyTerminalFact(ctx, tx, q, row, event)
	}
	if err != nil {
		return false, err
	}
	// Results are transport-compatible with command receipt replay; checkout's
	// original final_result is not rewritten when a later refund arrives.
	current, err := q.GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: row.TenantID, ID: row.ID})
	if err != nil {
		return false, store.MapError(err)
	}
	result, err := json.Marshal(snapshot(current, event.Status))
	if err != nil {
		return false, contract.ErrUnavailable
	}
	if !refund && current.FinalResult != nil {
		result = current.FinalResult
	}
	err = q.CompleteCheckoutAttempt(ctx, db.CompleteCheckoutAttemptParams{TenantID: row.TenantID, EventKind: "payment", EventKey: event.EventID, Result: result})
	return err == nil, store.MapError(err)
}

func (s *Service) applyTerminalFact(ctx context.Context, tx pgx.Tx, q *db.Queries, row db.AppCheckoutSession, event payment.VerifiedEvent) error {
	paid := event.Status == payment.StatusCaptured
	if row.Phase == "paid" {
		return nil // Late failures cannot undo captured funds; capture cannot repost.
	}
	if !paid && (row.Phase == "failed" || row.Phase == "cancelled") {
		return nil
	}
	phase, terminal, target := "cancelling", "failed", orders.Cancelled
	if paid {
		phase, terminal, target = "confirming", "paid", orders.Paid
	}
	if row.Phase == "ready" {
		row.Phase, row.ClaimKind, row.ClaimKey = phase, store.Text("payment"), store.Text(event.EventID)
		var err error
		row, err = update(ctx, q, row)
		if err != nil {
			return err
		}
	} else if row.Phase != phase {
		// In particular, never absorb capture after cancellation/failure, or
		// overwrite a durable competing confirmation with a failure fact.
		return contract.ErrConflict
	}
	changed, err := s.config.Orders.Transition(ctx, tx, row.TenantID.String(), orders.TransitionInput{OrderID: row.OrderID.String(), Target: target, EventKey: "checkout:" + row.ID.String() + ":" + terminal})
	if err != nil {
		return err
	}
	if changed.State != target {
		return contract.ErrConflict
	}
	if paid {
		posting, err := factJournal(ctx, q, row, row.ClaimKey.String)
		if err != nil {
			return err
		}
		if _, err = s.config.Ledger.Post(ctx, tx, posting); err != nil {
			return err
		}
	}
	row.Phase = terminal
	row.FinalResult, err = json.Marshal(snapshot(row, event.Status))
	if err != nil {
		return contract.ErrUnavailable
	}
	_, err = update(ctx, q, row)
	return err
}

func factJournal(ctx context.Context, q *db.Queries, row db.AppCheckoutSession, eventKey string) (ledger.Transaction, error) {
	rows, err := q.GetCheckoutLedgerAccounts(ctx, row.TenantID)
	if err != nil {
		return ledger.Transaction{}, store.MapError(err)
	}
	accounts := make(map[string]db.GetCheckoutLedgerAccountsRow, len(rows))
	for _, account := range rows {
		accounts[account.Code] = account
	}
	return journal(row, eventKey, accounts)
}

func refundParts(row db.AppCheckoutSession, delta money.Money) ([]money.Money, int64, error) {
	original := []int64{row.ItemsNetMinor, row.TaxMinor, row.ShippingMinor}
	refunded := []int64{row.RefundedNetMinor, row.RefundedTaxMinor, row.RefundedShippingMinor}
	remaining := make([]int64, 3)
	var used int64
	for i := range original {
		if refunded[i] < 0 || refunded[i] > original[i] {
			return nil, 0, contract.ErrConflict
		}
		remaining[i] = original[i] - refunded[i]
		// Each nonnegative component is bounded by the immutable total, so this
		// subtraction checks both the aggregate bound and addition overflow.
		if refunded[i] > row.AmountMinor-used {
			return nil, 0, contract.ErrConflict
		}
		used += refunded[i]
	}
	if delta.Currency != row.Currency || delta.Amount <= 0 || delta.Amount > row.AmountMinor-used {
		return nil, 0, contract.ErrConflict
	}
	parts, err := delta.Allocate(remaining)
	if err != nil {
		return nil, 0, contract.ErrInvalid
	}
	for i := range parts {
		if parts[i].Amount < 0 || parts[i].Amount > remaining[i] {
			return nil, 0, contract.ErrConflict
		}
	}
	return parts, used + delta.Amount, nil
}

func (s *Service) applyRefundFact(ctx context.Context, tx pgx.Tx, q *db.Queries, row db.AppCheckoutSession, event payment.VerifiedEvent) error {
	parts, cumulative, err := refundParts(row, event.Amount)
	if err != nil {
		return err
	}
	target := orders.PartiallyRefunded
	if cumulative == row.AmountMinor {
		target = orders.Refunded
	}
	refundKey := "refund:" + hex.EncodeToString(digest(event.EventID))
	changed, err := s.config.Orders.Transition(ctx, tx, row.TenantID.String(), orders.TransitionInput{OrderID: row.OrderID.String(), Target: target, EventKey: refundKey, RefundMinor: event.Amount.Amount})
	if err != nil {
		return err
	}
	if changed.State != target {
		return contract.ErrConflict
	}
	deltaRow := row
	deltaRow.AmountMinor, deltaRow.ItemsNetMinor, deltaRow.TaxMinor, deltaRow.ShippingMinor = event.Amount.Amount, parts[0].Amount, parts[1].Amount, parts[2].Amount
	posting, err := factJournal(ctx, q, deltaRow, refundKey)
	if err != nil {
		return err
	}
	for i := range posting.Entries {
		if posting.Entries[i].Side == ledger.Debit {
			posting.Entries[i].Side = ledger.Credit
		} else {
			posting.Entries[i].Side = ledger.Debit
		}
	}
	if _, err = s.config.Ledger.Post(ctx, tx, posting); err != nil {
		return err
	}
	_, err = q.UpdateCheckoutRefundTotals(ctx, db.UpdateCheckoutRefundTotalsParams{TenantID: row.TenantID, ID: row.ID, RefundedNetMinor: row.RefundedNetMinor + parts[0].Amount, RefundedTaxMinor: row.RefundedTaxMinor + parts[1].Amount, RefundedShippingMinor: row.RefundedShippingMinor + parts[2].Amount})
	return store.MapError(err)
}
