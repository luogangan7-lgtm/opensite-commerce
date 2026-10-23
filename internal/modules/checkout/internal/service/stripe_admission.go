// Responsibility: admit real-provider IO only from durable, accepted cart sessions.
// Dependencies: checkout queries and immutable Orders payable; caller: bootstrap's
// Stripe admission callback. No provider IO or claims occur in this transaction.
package service

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
	"time"
)

func (s *Service) AdmitRealPayment(ctx context.Context, in payment.CreatePaymentIntent, capture bool) error {
	if in.Validate() != nil || !s.available() {
		return contract.ErrUnavailable
	}
	return s.run(ctx, in.TenantID, func(ctx context.Context, tx pgx.Tx, q *db.Queries) error {
		r, err := q.GetCheckoutByOrder(ctx, db.GetCheckoutByOrderParams{TenantID: store.UUID(in.TenantID), OrderID: store.UUID(in.OrderID)})
		if err != nil {
			return store.MapError(err)
		}
		phase, action := "pending", "create"
		if capture {
			phase, action = "confirming", "capture"
		}
		var receipt struct {
			AcceptedTotal contract.AcceptedTotal `json:"accepted_total"`
		}
		if r.SourceKind != "cart" || r.Phase != phase || json.Unmarshal(r.AddressSnapshot, &receipt) != nil || receipt.AcceptedTotal.AmountMinor != r.AmountMinor || receipt.AcceptedTotal.Currency != r.Currency ||
			r.PayableVersion != in.OrderVersion || r.AmountMinor != in.Amount.Amount || r.Currency != in.Amount.Currency || in.IdempotencyKey != operation(r, action).IdempotencyKey {
			return contract.ErrTotalChanged
		}
		settings, err := q.GetCheckoutSettings(ctx, r.TenantID)
		// The cash account currency has the existing composite FK to the tenant's
		// settlement currency; no new grant to global tenant data is needed.
		if err != nil || settings.Currency != in.Amount.Currency {
			return contract.ErrConflict
		}
		if !capture {
			at, err := q.GetCheckoutServerTime(ctx)
			if err != nil {
				return err
			}
			if at.Time.Sub(r.CreatedAt.Time) > 23*time.Hour {
				return contract.ErrRetry
			}
		}
		p, err := s.config.Orders.GetPayable(ctx, tx, in.TenantID, in.OrderID)
		if err != nil {
			return err
		}
		if p.OrderID != in.OrderID || p.Version != in.OrderVersion || p.Amount != in.Amount || (r.ActorKind == "customer" && p.CustomerID != r.ActorID) {
			return contract.ErrConflict
		}
		return nil
	})
}
