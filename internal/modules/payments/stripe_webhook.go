// Responsibility: verify Stripe raw-byte signatures, then reuse mapped Inbox and
// atomic Checkout/Ledger fact application. Dependencies: HMAC-SHA256 and module
// ingress; callers: StripeRoute/CI. Timestamp is checked after DB locks, not OS time.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

func (s *Stripe) Receive(ctx context.Context, raw []byte, signature string) (contract.Receipt, error) {
	var zero contract.Receipt
	if len(raw) > 64<<10 || len(signature) > 4096 {
		return zero, invalid()
	}
	stamp := ""
	var supplied [][]byte
	for _, p := range strings.Split(signature, ",") {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return zero, rejected()
		}
		switch k {
		case "t":
			if stamp != "" {
				return zero, rejected()
			}
			stamp = v
		case "v1":
			b, e := hex.DecodeString(v)
			if e == nil && len(b) == 32 {
				supplied = append(supplied, b)
			}
		}
	}
	timestamp, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || timestamp <= 0 {
		return zero, rejected()
	}
	mac := hmac.New(sha256.New, s.config.Key)
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	valid := false
	for _, v := range supplied {
		valid = hmac.Equal(v, mac.Sum(nil)) || valid
	}
	if !valid {
		return zero, rejected()
	}
	var e struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Account  string `json:"account"`
		Livemode *bool  `json:"livemode"`
		Data     struct {
			Object stripeObject `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Livemode == nil || *e.Livemode || (e.Account != "" && e.Account != s.account) {
		return zero, invalid()
	}
	o := e.Data.Object
	status := map[string]payment.PaymentStatus{"payment_intent.succeeded": payment.StatusCaptured, "payment_intent.canceled": payment.StatusCanceled, "payment_intent.payment_failed": payment.StatusFailed, "refund.created": payment.StatusPartiallyRefunded, "refund.updated": payment.StatusPartiallyRefunded}[e.Type]
	if status == "" {
		return contract.Receipt{EventID: e.ID, Status: "ignored"}, nil
	}
	ref := o.ID
	refundID := ""
	if e.Type == "refund.created" || e.Type == "refund.updated" {
		if !strings.HasPrefix(o.ID, "re_") || len(o.ID) > 128 || !accountPattern.MatchString(o.ID) ||
			!strings.HasPrefix(o.PaymentIntent, "pi_") || len(o.PaymentIntent) > 200 || !stripeID.MatchString(o.PaymentIntent) ||
			!strings.HasPrefix(e.ID, "evt_") || len(e.ID) > 128 || !accountPattern.MatchString(e.ID) ||
			o.Amount <= 0 || (money.Money{Amount: o.Amount, Currency: strings.ToUpper(o.Currency)}).Validate() != nil {
			return zero, invalid()
		}
		if o.Status == "failed" || o.Status == "canceled" {
			return contract.Receipt{EventID: e.ID, Status: "ignored"}, nil
		}
		if o.Metadata["osc_order"] == "" || o.Status != "succeeded" {
			return s.unmatchedRefund(ctx, raw, e.ID, o.PaymentIntent, timestamp)
		}
		ref = o.PaymentIntent
		refundID = o.ID
	}
	if o.Metadata["osc_order"] == "" {
		return contract.Receipt{EventID: e.ID, Status: "ignored"}, nil
	}
	version, err := strconv.ParseInt(o.Metadata["osc_version"], 10, 64)
	if err != nil {
		return zero, invalid()
	}
	in := envelope{EventID: e.ID, AccountID: s.account, OrderID: o.Metadata["osc_order"], OrderVersion: version, ProviderRef: ref, Status: status, AmountMinor: o.Amount, Currency: strings.ToUpper(o.Currency), RefundID: refundID}
	if !strings.HasPrefix(ref, "pi_") || !stripeID.MatchString(ref) || !strings.HasPrefix(e.ID, "evt_") || len(e.ID) > 128 ||
		(payment.VerifiedEvent{EventID: e.ID, TenantID: "00000000-0000-0000-0000-000000000001", OrderID: in.OrderID, OrderVersion: version, ProviderRef: ref, Status: status, Amount: money.Money{Amount: o.Amount, Currency: in.Currency}}).Validate() != nil {
		return zero, invalid()
	}
	return s.Module.receive(ctx, raw, in, timestamp)
}

// Keep a bounded event identity/hash in the existing reconciliation inbox, not
// the raw payload. The provider event ID retrieves the full fact during matching.
// No domain consumer is called until trusted order/attempt matching exists.
func (s *Stripe) unmatchedRefund(ctx context.Context, raw []byte, event, ref string, stamp int64) (contract.Receipt, error) {
	hash := sha256.Sum256(raw)
	err := s.config.Run(ctx, s.account, func(ctx context.Context, tx pgx.Tx, _ string, at time.Time) error {
		if delta := at.Sub(time.Unix(stamp, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
			return rejected()
		}
		q := db.New(tx)
		locked, err := q.LockPaymentStream(ctx, db.LockPaymentStreamParams{Account: s.account, ProviderRef: ref})
		if err != nil {
			return err
		}
		if delta := locked.Time.Sub(time.Unix(stamp, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
			return rejected()
		}
		inbox, err := q.AdmitPaymentInbox(ctx, db.AdmitPaymentInboxParams{Account: s.account, EventID: event, PayloadHash: hash[:]})
		if err != nil {
			return err
		}
		return q.FinishPaymentInbox(ctx, db.FinishPaymentInboxParams{ID: inbox, Success: false})
	})
	if err != nil {
		return contract.Receipt{}, apperrors.New(503, "payment_unavailable", "Unmatched refund retention unavailable")
	}
	slog.WarnContext(ctx, "payment_webhook_reconciliation_required", "reason", "unmatched_refund", "event_id", event)
	return contract.Receipt{EventID: event, Status: "unmatched"}, nil
}

func StripeRoute(receiver contract.Receiver) httpapi.Route {
	return httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "receiveStripePaymentEvent", Method: "POST", Pattern: "/api/v1/payments/stripe/webhook", Access: httpapi.Public, MaxBodySize: 64 << 10}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if receiver == nil {
			httpapi.WriteProblem(w, r, apperrors.New(503, "payments_unavailable", "Stripe sandbox unavailable"))
			return
		}
		values := r.Header.Values("Stripe-Signature")
		if len(values) != 1 {
			httpapi.WriteProblem(w, r, rejected())
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			httpapi.WriteProblem(w, r, apperrors.New(413, "request_too_large", "Payment event exceeds limit"))
			return
		}
		receipt, err := receiver.Receive(r.Context(), raw, values[0])
		if err != nil {
			httpapi.WriteProblem(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(receipt)
	})}
}
