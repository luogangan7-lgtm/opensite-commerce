// Responsibility: recover retained Stripe terminal facts without a second money path.
// Dependencies: trusted Stripe retrieval and generated closed SQL ports; callers:
// supervised operator composition. Invariants: no provider IO under DB locks,
// exact original Inbox hash, current-object confirmation, no automatic re-refund.
package payments

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

// ReplayRefundCase retrieves an immutable provider event and the current refund.
// It admits no operator amounts/order metadata; SQL derives the persisted binding.
func (s *Stripe) ReplayRefundCase(ctx context.Context, eventID string) (contract.Receipt, error) {
	out := contract.Receipt{EventID: eventID, Status: "unmatched"}
	if !strings.HasPrefix(eventID, "evt_") || len(eventID) > 128 || !accountPattern.MatchString(eventID) || s.config.TerminalRun == nil {
		return out, invalid()
	}
	var evidence struct {
		CaseID      string `json:"case_id"`
		Hash        string `json:"payload_hash"`
		RefundID    string `json:"refund_id"`
		ProviderRef string `json:"provider_ref"`
	}
	err := s.config.TerminalRun(ctx, s.account, func(ctx context.Context, tx pgx.Tx, _ string, _ time.Time) error {
		raw, err := db.New(tx).RefundCaseEvidence(ctx, db.RefundCaseEvidenceParams{EventID: eventID, AccountID: s.account})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &evidence)
	})
	if err != nil || !id(evidence.CaseID).Valid {
		return out, apperrors.New(409, "payment_conflict", "Retained refund evidence unavailable")
	}
	var event struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Account  string `json:"account"`
		Livemode *bool  `json:"livemode"`
		Data     struct {
			Object stripeObject `json:"object"`
		} `json:"data"`
	}
	if err := s.request(ctx, "GET", "events/"+eventID, "", nil, &event); err != nil {
		return out, err
	}
	o := event.Data.Object
	if event.ID != eventID || event.Livemode == nil || *event.Livemode || (event.Account != "" && event.Account != s.account) ||
		(event.Type != "refund.created" && event.Type != "refund.updated" && event.Type != "refund.failed") ||
		(evidence.RefundID != "" && o.ID != evidence.RefundID) || (evidence.ProviderRef != "" && o.PaymentIntent != evidence.ProviderRef) || !strings.HasPrefix(o.ID, "re_") || !accountPattern.MatchString(o.ID) {
		return out, invalid()
	}
	var current stripeObject
	if err := s.request(ctx, "GET", "refunds/"+o.ID, "", nil, &current); err != nil {
		return out, err
	}
	if current.ID != o.ID || current.PaymentIntent != o.PaymentIntent || current.Amount != o.Amount || current.Currency != o.Currency ||
		(current.Livemode != nil && *current.Livemode) || (current.Status != "failed" && current.Status != "canceled") {
		return out, apperrors.New(409, "payment_conflict", "Current terminal refund is unconfirmed")
	}
	var receipt contract.CompensationReceipt
	err = s.config.TerminalRun(ctx, s.account, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
		fact, err := json.Marshal(contract.TerminalObservation{TenantID: tenant, Provider: "stripe", AccountID: s.account, RefundID: current.ID,
			EventID: eventID, PayloadHash: evidence.Hash, ProviderRef: current.PaymentIntent, Status: current.Status, AmountMinor: current.Amount, Currency: strings.ToUpper(current.Currency)})
		if err != nil {
			return err
		}
		raw, err := db.New(tx).ReplayRefundCase(ctx, db.ReplayRefundCaseParams{CaseID: id(evidence.CaseID), Fact: fact})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &receipt)
	})
	if err != nil {
		return out, apperrors.New(503, "payment_unavailable", "Refund recovery unavailable")
	}
	if receipt.ReconciliationRequired {
		return out, apperrors.New(409, "payment_conflict", "Refund binding requires reconciliation")
	}
	slog.InfoContext(ctx, "refund_compensation_completed", "case_id", evidence.CaseID)
	out.Status = "processed"
	return out, nil
}

// OperatorResolution contains authenticated credential identifiers, never secrets.
// The closed SQL port revalidates owner/admin returns:write under authority locks.
type OperatorResolution struct {
	UserID       string `json:"user_id"`
	Kind         string `json:"kind"`
	CredentialID string `json:"credential_id"`
	Reason       string `json:"reason"`
	EvidenceHash string `json:"evidence_hash"`
}

func ResolveRefundCase(ctx context.Context, tx pgx.Tx, caseID string, resolution OperatorResolution) error {
	if tx == nil || !id(caseID).Valid || uuid.Validate(resolution.UserID) != nil || uuid.Validate(resolution.CredentialID) != nil {
		return invalid()
	}
	raw, err := json.Marshal(resolution)
	if err != nil {
		return err
	}
	return db.New(tx).ResolveRefundCase(ctx, db.ResolveRefundCaseParams{CaseID: id(caseID), Resolution: raw})
}
