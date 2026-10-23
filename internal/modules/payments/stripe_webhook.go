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
	"net/http"
	"strconv"
	"strings"

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
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
	status := map[string]payment.PaymentStatus{"payment_intent.succeeded": payment.StatusCaptured, "payment_intent.canceled": payment.StatusCanceled, "payment_intent.payment_failed": payment.StatusFailed, "refund.created": payment.StatusPartiallyRefunded}[e.Type]
	if status == "" || o.Metadata["osc_order"] == "" {
		return contract.Receipt{EventID: e.ID, Status: "ignored"}, nil
	}
	ref := o.ID
	if e.Type == "refund.created" {
		if o.Status != "succeeded" {
			return contract.Receipt{EventID: e.ID, Status: "ignored"}, nil
		}
		ref = o.PaymentIntent
	}
	version, err := strconv.ParseInt(o.Metadata["osc_version"], 10, 64)
	if err != nil {
		return zero, invalid()
	}
	in := envelope{EventID: e.ID, AccountID: s.account, OrderID: o.Metadata["osc_order"], OrderVersion: version, ProviderRef: ref, Status: status, AmountMinor: o.Amount, Currency: strings.ToUpper(o.Currency)}
	if !strings.HasPrefix(ref, "pi_") || !stripeID.MatchString(ref) || !strings.HasPrefix(e.ID, "evt_") || len(e.ID) > 128 ||
		(payment.VerifiedEvent{EventID: e.ID, TenantID: "00000000-0000-0000-0000-000000000001", OrderID: in.OrderID, OrderVersion: version, ProviderRef: ref, Status: status, Amount: money.Money{Amount: o.Amount, Currency: in.Currency}}).Validate() != nil {
		return zero, invalid()
	}
	return s.Module.receive(ctx, raw, in, timestamp)
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
