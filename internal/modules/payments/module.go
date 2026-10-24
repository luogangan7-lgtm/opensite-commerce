// Package payments admits authenticated Fake facts into the existing inbox and
// applies them via a caller-transaction port. Dependencies: generated SQL, neutral
// Payment events, stdlib HMAC, mapped pgx transaction; callers: bootstrap/HTTP.
// Invariants: DB account/time authority, immutable event identity, no PSP commands,
// receipt and effects atomic, conflicting facts retained without poisoning peers.
package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Config = contract.Config
type Module struct{ config Config }
type envelope struct {
	EventID      string                `json:"event_id"`
	AccountID    string                `json:"account_id"`
	OrderID      string                `json:"order_id"`
	OrderVersion int64                 `json:"order_version"`
	ProviderRef  string                `json:"provider_ref"`
	Status       payment.PaymentStatus `json:"status"`
	AmountMinor  int64                 `json:"amount_minor"`
	Currency     string                `json:"currency"`
	RefundID     string                `json:"refund_id,omitempty"`
}

var accountPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var signaturePattern = regexp.MustCompile(`^t=([0-9]{1,12}),v1=([0-9a-f]{64})$`)

func New(config Config) (*Module, error) {
	if config.Provider == "" {
		config.Provider = "fake"
	}
	if config.Provider != "fake" && config.Provider != "stripe" {
		return nil, errors.New("unsupported payment provider")
	}
	if len(config.Key) < 32 || config.Run == nil || config.Apply == nil {
		return nil, errors.New("payment ingress requires key, mapped transaction and fact consumer")
	}
	config.Key = bytes.Clone(config.Key)
	return &Module{config: config}, nil
}
func rejected() error {
	return apperrors.New(401, "unauthenticated", "Payment webhook authentication failed")
}
func invalid() error          { return apperrors.New(400, "invalid_event", "Invalid payment event") }
func id(s string) pgtype.UUID { u, e := uuid.Parse(s); return pgtype.UUID{Bytes: u, Valid: e == nil} }

func (m *Module) verify(raw []byte, signature string) (envelope, int64, error) {
	var in envelope
	if len(raw) > 64<<10 {
		return in, 0, apperrors.New(413, "request_too_large", "Payment event exceeds limit")
	}
	parts := signaturePattern.FindStringSubmatch(signature)
	if len(parts) != 3 {
		return in, 0, rejected()
	}
	timestamp, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil {
		return in, 0, rejected()
	}
	supplied, _ := hex.DecodeString(parts[2])
	mac := hmac.New(sha256.New, m.config.Key)
	_, _ = mac.Write([]byte(parts[1] + "."))
	_, _ = mac.Write(raw)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return in, 0, rejected()
	}
	// The wire is a flat object. Reject duplicate keys even for non-HTTP callers.
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return in, 0, invalid()
	}
	seen := map[string]bool{}
	for d.More() {
		tok, e = d.Token()
		if e != nil {
			return in, 0, invalid()
		}
		name, ok := tok.(string)
		if !ok || seen[name] {
			return in, 0, invalid()
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return in, 0, invalid()
		}
	}
	if _, e = d.Token(); e != nil {
		return in, 0, invalid()
	}
	if _, e = d.Token(); e != io.EOF {
		return in, 0, invalid()
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil {
		return in, 0, invalid()
	}
	if !accountPattern.MatchString(in.AccountID) || len(in.ProviderRef) > 200 || strings.ContainsAny(in.ProviderRef, "\r\n\x00") {
		return in, 0, invalid()
	}
	if len(in.RefundID) > 200 || strings.ContainsFunc(in.RefundID, func(c rune) bool { return c < '!' || c > '~' }) ||
		(in.RefundID != "" && in.Status != payment.StatusPartiallyRefunded && in.Status != payment.StatusRefunded && in.Status != payment.StatusFailed && in.Status != payment.StatusCanceled) {
		return in, 0, invalid()
	}
	switch in.Status {
	case payment.StatusCaptured, payment.StatusFailed, payment.StatusCanceled, payment.StatusPartiallyRefunded, payment.StatusRefunded:
	default:
		return in, 0, invalid()
	}
	// A synthetic tenant is only a shape witness. Actual authority is supplied by
	// the mapped transaction below, never by any webhook field.
	shape := payment.VerifiedEvent{TenantID: "00000000-0000-0000-0000-000000000001", EventID: in.EventID, OrderID: in.OrderID, OrderVersion: in.OrderVersion, ProviderRef: in.ProviderRef, Status: in.Status, Amount: money.Money{Amount: in.AmountMinor, Currency: in.Currency}}
	if shape.Validate() != nil {
		return in, 0, invalid()
	}
	return in, timestamp, nil
}
func (m *Module) Receive(ctx context.Context, raw []byte, signature string) (contract.Receipt, error) {
	in, stamp, err := m.verify(raw, signature)
	if err != nil {
		return contract.Receipt{}, err
	}
	if in.RefundID != "" && (in.Status == payment.StatusFailed || in.Status == payment.StatusCanceled) {
		return m.receiveTerminal(ctx, raw, in, stamp)
	}
	return m.receive(ctx, raw, in, stamp)
}

func (m *Module) receive(ctx context.Context, raw []byte, in envelope, stamp int64) (contract.Receipt, error) {
	receipt := contract.Receipt{EventID: in.EventID, Status: "pending"}
	var captures []payment.VerifiedEvent
	payload := sha256.Sum256(raw)
	identity, _ := json.Marshal([4]string{m.config.Provider, "test", in.AccountID, in.EventID})
	if in.RefundID != "" {
		identity, _ = json.Marshal([5]string{m.config.Provider, "test", in.AccountID, "refund", in.RefundID})
	}
	eventKey := sha256.Sum256(identity)
	err := m.config.Run(ctx, in.AccountID, func(ctx context.Context, tx pgx.Tx, tenant string, at time.Time) error {
		captures = nil // Retain only this transaction attempt if a runner retries.
		delta := at.Sub(time.Unix(stamp, 0))
		if delta > 5*time.Minute || delta < -5*time.Minute {
			return rejected()
		}
		q := db.New(tx)
		lockedAt, e := q.LockPaymentStream(ctx, db.LockPaymentStreamParams{Account: in.AccountID, ProviderRef: in.ProviderRef})
		if e != nil {
			return e
		}
		// A queued delivery must still be fresh when it actually gains admission.
		delta = lockedAt.Time.Sub(time.Unix(stamp, 0))
		if delta > 5*time.Minute || delta < -5*time.Minute {
			return rejected()
		}
		inbox, e := q.AdmitPaymentInbox(ctx, db.AdmitPaymentInboxParams{Account: in.AccountID, EventID: in.EventID, PayloadHash: payload[:]})
		if e != nil {
			return e
		}
		if in.RefundID == "" && (in.Status == payment.StatusPartiallyRefunded || in.Status == payment.StatusRefunded) {
			receipt.Status = "rejected"
			return q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: false})
		}
		key := hex.EncodeToString(eventKey[:])
		if in.RefundID != "" {
			prior, err := q.GetPaymentEvent(ctx, db.GetPaymentEventParams{TenantID: id(tenant), ID: inbox})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil && !prior.ProviderRefundID.Valid {
				// Exact-body replay of a legacy delivery retains its old journal key.
				key = prior.EventKey
			} else {
				legacy, err := q.HasLegacyPaymentRefund(ctx, db.HasLegacyPaymentRefundParams{TenantID: id(tenant), ProviderAccountID: in.AccountID, ProviderRef: in.ProviderRef, Provider: m.config.Provider})
				if err != nil {
					return err
				}
				if legacy {
					// No payload/refund ID survived the old parser. Retain the new
					// delivery for reconciliation rather than infer its money identity.
					receipt.Status = "rejected"
					if err := q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: false}); err != nil {
						return err
					}
					return retainRefundCase(ctx, q, in, payload, "unmatched_refund")
				}
			}
		}
		e = q.InsertPaymentEvent(ctx, db.InsertPaymentEventParams{TenantID: id(tenant), ID: inbox, ProviderAccountID: in.AccountID, ProviderRef: in.ProviderRef, EventKey: key, ClaimedOrderID: id(in.OrderID), OrderVersion: in.OrderVersion, Status: string(in.Status), AmountMinor: in.AmountMinor, Currency: in.Currency, ProviderRefundID: pgtype.Text{String: in.RefundID, Valid: in.RefundID != ""}})
		if e != nil {
			return e
		}
		bound, e := q.GetPaymentEventByKey(ctx, db.GetPaymentEventByKeyParams{TenantID: id(tenant), EventKey: key})
		if e != nil {
			return e
		}
		if bound.ProviderRef != in.ProviderRef || bound.ClaimedOrderID != id(in.OrderID) || bound.OrderVersion != in.OrderVersion || bound.Status != string(in.Status) || bound.AmountMinor != in.AmountMinor || bound.Currency != in.Currency {
			return apperrors.New(409, "payment_conflict", "Refund fact identity is bound to different content")
		}
		if e = q.BindPaymentDelivery(ctx, db.BindPaymentDeliveryParams{TenantID: id(tenant), ID: inbox, PaymentEventID: bound.ID}); e != nil {
			return e
		}
		rows, e := q.ListPendingPaymentEvents(ctx, db.ListPendingPaymentEventsParams{TenantID: id(tenant), ProviderAccountID: in.AccountID, ProviderRef: in.ProviderRef})
		if e != nil {
			return e
		}
		for _, r := range rows {
			// A bad but authenticated provider fact is retained for reconciliation.
			// SAVEPOINT prevents its failed domain operation from poisoning valid peers.
			child, e := tx.Begin(ctx)
			if e != nil {
				return e
			}
			event := payment.VerifiedEvent{EventID: r.EventKey, TenantID: tenant, OrderID: r.ClaimedOrderID.String(), OrderVersion: r.OrderVersion, ProviderRef: r.ProviderRef, Status: payment.PaymentStatus(r.Status), Amount: money.Money{Amount: r.AmountMinor, Currency: r.Currency}}
			done, applyErr := m.applyFact(ctx, child, event, r.ProviderRefundID.String, r.ProviderAccountID)
			if done && applyErr == nil && event.Status == payment.StatusCaptured {
				captures = append(captures, event)
			}
			var code pgtype.Text
			if applyErr != nil {
				if e = child.Rollback(ctx); e != nil {
					return e
				}
				var conflict *payment.PaymentError
				if !errors.As(applyErr, &conflict) || conflict.Code != payment.ErrorStateConflict {
					return applyErr
				}
				code = pgtype.Text{String: "state_conflict", Valid: true}
			} else if e = child.Commit(ctx); e != nil {
				return e
			}
			if !done && !code.Valid {
				continue
			}
			if e = q.SetPaymentEventOutcome(ctx, db.SetPaymentEventOutcomeParams{TenantID: r.TenantID, ID: r.ID, Applied: done, ErrorCode: code}); e != nil {
				return e
			}
			if e = q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: r.ID, Success: done}); e != nil {
				return e
			}
			aliases, e := q.ListPaymentDeliveries(ctx, db.ListPaymentDeliveriesParams{TenantID: r.TenantID, PaymentEventID: r.ID})
			if e != nil {
				return e
			}
			for _, alias := range aliases {
				if e = q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: alias.ID, Success: done}); e != nil {
					return e
				}
				if code.Valid && r.ProviderRefundID.Valid {
					// A different incoming event may drain this queued refund. Its
					// aliases retain their own immutable evidence, never that event's.
					fact := envelope{AccountID: r.ProviderAccountID, ProviderRef: r.ProviderRef,
						RefundID: r.ProviderRefundID.String, EventID: alias.ProviderEventID}
					if e = retainRefundCase(ctx, q, fact, [32]byte(alias.PayloadHash), "unmatched_refund"); e != nil {
						return e
					}
				}
			}
		}
		current, e := q.GetPaymentEventByKey(ctx, db.GetPaymentEventByKeyParams{TenantID: id(tenant), EventKey: key})
		if e != nil {
			return e
		}
		if current.Applied {
			// Include replay: a previous post-commit observer may have failed.
			if current.Status == string(payment.StatusCaptured) {
				captures = append(captures, payment.VerifiedEvent{EventID: current.EventKey, TenantID: tenant, OrderID: current.ClaimedOrderID.String(), OrderVersion: current.OrderVersion, ProviderRef: current.ProviderRef, Status: payment.StatusCaptured, Amount: money.Money{Amount: current.AmountMinor, Currency: current.Currency}})
			}
			receipt.Status = "processed"
		} else if current.ErrorCode.Valid {
			receipt.Status = "rejected"
			if err := retainRefundCase(ctx, q, in, payload, "unmatched_refund"); err != nil {
				return err
			}
		}
		if current.ID != inbox && (current.Applied || current.ErrorCode.Valid) {
			return q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: current.Applied})
		}
		return nil
	})
	if err != nil {
		var pe *pgxadapter.PgError
		if errors.As(err, &pe) {
			switch pe.Code {
			case "23505":
				return receipt, apperrors.New(409, "idempotency_key_reused", "Payment event identity is bound to different content")
			case "23503":
				return receipt, apperrors.New(404, "not_found", "Payment binding not found")
			}
		}
		var public *apperrors.Error
		if errors.As(err, &public) {
			if public.Status == 401 {
				slog.WarnContext(ctx, "payment_webhook_rejected", "reason", "account_or_timestamp")
			}
			return receipt, err
		}
		return receipt, apperrors.New(503, "payment_unavailable", "Payment event processing unavailable")
	}
	// No provider mutation before Run's successful commit. Rejected, unmatched
	// and rolled-back captures can never authorize the simulation's refund.
	if m.config.Provider == "fake" && m.config.ObserveCommittedCapture != nil {
		for _, event := range captures {
			if err := m.config.ObserveCommittedCapture(event); err != nil {
				return receipt, apperrors.New(503, "payment_unavailable", "Fake capture observation unavailable")
			}
		}
	}
	if receipt.Status == "rejected" {
		slog.WarnContext(ctx, "payment_webhook_reconciliation_required", "reason", "state_conflict")
		return receipt, apperrors.New(409, "payment_conflict", "Payment event requires reconciliation")
	}
	return receipt, nil
}
