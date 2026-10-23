// Responsibility: typed preparation contract for the future Payments finalizer.
// Dependencies: Ledger values only; caller: Payments, never generic order transition.
// Invariants: one failed/canceled transition key, immutable ordered reversal,
// no IO, posting, state update or automatic replacement refund in this layer.
package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
)

const RefundCompensationTransition = "posted_to_compensated/v1"

// RefundIdentity is resolved from durable authenticated bindings, never metadata.
// Environment is fixed to test while production enablement remains separately gated.
type RefundIdentity struct {
	TenantID, Provider, AccountID, RefundID string
}

func (r RefundIdentity) Validate() error {
	if !ledger.ValidID(r.TenantID) || (r.Provider != "fake" && r.Provider != "stripe") || !refundASCII(r.AccountID, 128) || !refundASCII(r.RefundID, 200) {
		return ledger.ErrInvalid
	}
	for _, c := range r.AccountID {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ledger.ErrInvalid
		}
	}
	return nil
}
func refundASCII(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range []byte(s) {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// ReversalSourceID hashes a length-prefixed ASCII tuple into the bounded Ledger
// source identity. Tenant is already part of Ledger's unique key. SQL uses the
// same byte encoding; raw event IDs and failure reason never participate.
func (r RefundIdentity) ReversalSourceID() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	key := fmt.Sprintf("%d:%s%d:%s%d:%s", len(r.Provider), r.Provider, len(r.AccountID), r.AccountID, len(r.RefundID), r.RefundID)
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:]), nil
}

// Reversal copies persisted original entries. It cannot authenticate their origin;
// the future closed finalizer and independent posted-witness SQL guard must do so.
func (r RefundIdentity) Reversal(original ledger.Transaction) (ledger.Transaction, error) {
	source, err := r.ReversalSourceID()
	if err != nil {
		return ledger.Transaction{}, err
	}
	if err = original.Validate(); err != nil {
		return ledger.Transaction{}, err
	}
	if original.TenantID != r.TenantID {
		return ledger.Transaction{}, ledger.ErrForbidden
	}
	out := ledger.Transaction{TenantID: r.TenantID, Currency: original.Currency, SourceType: "payment_refund", SourceID: source, Event: RefundCompensationTransition, Entries: append([]ledger.Entry(nil), original.Entries...)}
	for i := range out.Entries {
		if out.Entries[i].Side == ledger.Debit {
			out.Entries[i].Side = ledger.Credit
		} else {
			out.Entries[i].Side = ledger.Debit
		}
	}
	return out, nil
}

// CompensationInput carries references, not an arbitrary amount or target state.
// Revision optionally fences the current Orders projection under the common lock.
// A value alone grants no authority: implementations must use the dedicated pool.
type CompensationInput struct {
	Identity          RefundIdentity
	OriginalJournalID string
	Revision          int64
	Observation       TerminalObservation
}
type TerminalObservation struct {
	TenantID          string `json:"tenant_id"`
	Provider          string `json:"provider"`
	AccountID         string `json:"account_id"`
	RefundID          string `json:"refund_id"`
	EventID           string `json:"event_id"`
	OrderID           string `json:"order_id"`
	OrderVersion      int64  `json:"order_version"`
	ProviderRef       string `json:"provider_ref"`
	Status            string `json:"status"`
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
	PayloadHash       string `json:"payload_hash"`
	Timestamp         int64  `json:"timestamp"`
	OriginalJournalID string `json:"original_journal_id"`
	Revision          int64  `json:"revision"`
}
type CompensationReceipt struct {
	OriginalJournalID string `json:"original_journal_id"`
	ReversalJournalID string `json:"reversal_journal_id"`
}

// RefundCompensator commits through Payments and closed domain projection ports.
// It owns the complete journal/projection/case/outbox transaction, never a partial
// journal command. Provider retrieval must happen before entering this method.
type RefundCompensator interface {
	Compensate(context.Context, pgx.Tx, CompensationInput) (CompensationReceipt, error)
}
