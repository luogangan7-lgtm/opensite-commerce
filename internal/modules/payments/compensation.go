// Responsibility: bind authenticated terminal observations to the closed finalizer.
// Dependencies: generated SQL and public compensation contract; callers: signed ingress.
// Invariants: no provider IO under transaction locks; domain SQL ports commit together.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
)

type Finalizer struct{}

var _ contract.RefundCompensator = Finalizer{}

func (Finalizer) Compensate(ctx context.Context, tx pgx.Tx, in contract.CompensationInput) (contract.CompensationReceipt, error) {
	var out contract.CompensationReceipt
	if tx == nil || in.Identity.Validate() != nil {
		return out, invalid()
	}
	fact := in.Observation
	fact.TenantID, fact.Provider = in.Identity.TenantID, in.Identity.Provider
	fact.AccountID, fact.RefundID = in.Identity.AccountID, in.Identity.RefundID
	fact.OriginalJournalID, fact.Revision = in.OriginalJournalID, in.Revision
	raw, err := json.Marshal(fact)
	if err != nil {
		return out, err
	}
	result, err := db.New(tx).FinalizeRefundCompensation(ctx, raw)
	if err == nil {
		err = json.Unmarshal(result, &out)
	}
	return out, err
}

func (m *Module) receiveTerminal(ctx context.Context, raw []byte, in envelope, stamp int64) (contract.Receipt, error) {
	out := contract.Receipt{EventID: in.EventID, Status: "rejected"}
	if m.config.TerminalRun == nil {
		return out, errors.New("refund compensation capability unavailable")
	}
	hash := sha256.Sum256(raw)
	err := m.config.TerminalRun(ctx, in.AccountID, func(ctx context.Context, tx pgx.Tx, tenant string, _ time.Time) error {
		_, err := (Finalizer{}).Compensate(ctx, tx, contract.CompensationInput{
			Identity: contract.RefundIdentity{TenantID: tenant, Provider: m.config.Provider, AccountID: in.AccountID, RefundID: in.RefundID},
			Observation: contract.TerminalObservation{EventID: in.EventID, OrderID: in.OrderID, OrderVersion: in.OrderVersion,
				ProviderRef: in.ProviderRef, Status: string(in.Status), AmountMinor: in.AmountMinor, Currency: in.Currency,
				PayloadHash: hex.EncodeToString(hash[:]), Timestamp: stamp},
		})
		return err
	})
	if err == nil {
		out.Status = "processed"
	}
	return out, err
}
