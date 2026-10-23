// Responsibility: Stripe TEST MODE implementation of the existing Payment Port.
// Dependencies: net/http, env-only credentials, mapped DB account and Checkout;
// callers: bootstrap/CI. No live keys, customer data, redirect or endpoint override.
// Provider errors are bounded; uncertain effects never get NoSideEffect proof.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
)

type Stripe struct {
	*Module
	secret, account, method string
	client                  *http.Client
}
type stripeObject struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Currency       string            `json:"currency"`
	Amount         int64             `json:"amount"`
	AmountRefunded int64             `json:"amount_refunded"`
	Livemode       *bool             `json:"livemode"`
	Metadata       map[string]string `json:"metadata"`
	PaymentIntent  string            `json:"payment_intent"`
	LatestCharge   json.RawMessage   `json:"latest_charge"`
}

var stripeID = regexp.MustCompile(`^(pi|acct)_[A-Za-z0-9]+$`)
var _ payment.Port = (*Stripe)(nil)

func NewStripe(c Config) (*Stripe, error) {
	secret, whsec, account := os.Getenv("STRIPE_TEST_SECRET_KEY"), os.Getenv("STRIPE_TEST_WEBHOOK_SECRET"), os.Getenv("STRIPE_TEST_ACCOUNT_ID")
	if !strings.HasPrefix(secret, "sk_test_") || len(secret) <= 8 || strings.ContainsAny(secret, "\r\n\x00") {
		return nil, errors.New("Stripe requires a test secret key")
	}
	if e := os.Getenv("OPENSITE_ENVIRONMENT"); e != "test" && e != "development" {
		return nil, errors.New("Stripe sandbox forbidden outside test/development")
	}
	if !strings.HasPrefix(whsec, "whsec_") || len(whsec) < 32 || !strings.HasPrefix(account, "acct_") || !stripeID.MatchString(account) || c.Admit == nil {
		return nil, errors.New("Stripe sandbox requires signing, account and consent configuration")
	}
	method := os.Getenv("STRIPE_TEST_PAYMENT_METHOD")
	if method == "" {
		method = "pm_card_visa"
	}
	if method != "pm_card_visa" && method != "pm_card_chargeDeclined" {
		return nil, errors.New("Stripe sandbox requires an approved test PaymentMethod")
	}
	c.Provider, c.Key = "stripe", []byte(whsec)
	m, err := New(c)
	if err != nil {
		return nil, err
	}
	return &Stripe{Module: m, secret: secret, account: account, method: method, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func stripeError(code payment.ErrorCode) error {
	return &payment.PaymentError{Code: code, Message: "Stripe sandbox operation unavailable"}
}
func stripeKey(r payment.CreatePaymentIntent, op string) string {
	sum := sha256.Sum256([]byte(r.TenantID + ":" + op + ":" + r.IdempotencyKey))
	return hex.EncodeToString(sum[:])
}
func stripeMetadata(r payment.CreatePaymentIntent, op string) url.Values {
	return url.Values{"metadata[osc_tenant]": {r.TenantID}, "metadata[osc_order]": {r.OrderID}, "metadata[osc_version]": {strconv.FormatInt(r.OrderVersion, 10)}, "metadata[osc_key]": {stripeKey(r, op)}}
}
func (s *Stripe) request(ctx context.Context, method, path, key string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com/v1/"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return stripeError(payment.ErrorNonRetryable)
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Stripe-Version", "2026-08-26.dahlia")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return stripeError(payment.ErrorRetryable)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return stripeError(payment.ErrorRetryable)
	}
	if res.StatusCode >= 400 {
		code := payment.ErrorRetryable
		if res.StatusCode == 402 {
			code = payment.ErrorNonRetryable
		} // confirmed card decline, never an ambiguous transport failure
		if res.StatusCode == 409 {
			code = payment.ErrorIdempotencyConflict
		}
		if res.StatusCode == 404 {
			code = payment.ErrorNotFound
		}
		return stripeError(code)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || json.Unmarshal(raw, out) != nil {
		return stripeError(payment.ErrorRetryable)
	}
	return nil
}

// The existing resolver locks an enabled stripe/test mapping and commits before
// any HTTP. Provider-account configuration never supplies tenant authority.
func (s *Stripe) admitAccount(ctx context.Context, r payment.CreatePaymentIntent) error {
	if r.Validate() != nil {
		return stripeError(payment.ErrorNonRetryable)
	}
	err := s.config.Run(ctx, s.account, func(_ context.Context, _ pgx.Tx, tenant string, _ time.Time) error {
		if tenant != r.TenantID {
			return stripeError(payment.ErrorStateConflict)
		}
		return nil
	})
	if err != nil {
		return stripeError(payment.ErrorStateConflict)
	}
	return nil
}
func (s *Stripe) intent(ctx context.Context, r payment.PaymentOperation) (stripeObject, error) {
	var o stripeObject
	if r.Validate() != nil || !strings.HasPrefix(r.ProviderRef, "pi_") || !stripeID.MatchString(r.ProviderRef) {
		return o, stripeError(payment.ErrorNonRetryable)
	}
	if err := s.admitAccount(ctx, r.CreatePaymentIntent); err != nil {
		return o, err
	}
	err := s.request(ctx, "GET", "payment_intents/"+r.ProviderRef+"?expand[]=latest_charge", "", nil, &o)
	if err == nil && (o.ID != r.ProviderRef || o.Livemode == nil || *o.Livemode || o.Metadata["osc_tenant"] != r.TenantID || o.Metadata["osc_order"] != r.OrderID || o.Metadata["osc_version"] != strconv.FormatInt(r.OrderVersion, 10) || strings.ToUpper(o.Currency) != r.Amount.Currency) {
		err = stripeError(payment.ErrorAmountMismatch)
	}
	return o, err
}
func stripeResult(o stripeObject) (payment.PaymentIntentResult, error) {
	status := map[string]payment.PaymentStatus{"requires_payment_method": payment.StatusPending, "requires_confirmation": payment.StatusPending, "processing": payment.StatusPending, "requires_action": payment.StatusRequiresAction, "requires_capture": payment.StatusAuthorized, "succeeded": payment.StatusCaptured, "canceled": payment.StatusCanceled}[o.Status]
	if status == "" || o.Livemode == nil || *o.Livemode || !strings.HasPrefix(o.ID, "pi_") || !stripeID.MatchString(o.ID) {
		return payment.PaymentIntentResult{}, stripeError(payment.ErrorStateConflict)
	}
	return payment.PaymentIntentResult{ProviderRef: o.ID, Status: status}, nil
}
func (s *Stripe) CreateIntent(ctx context.Context, r payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	if err := s.admitAccount(ctx, r); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if err := s.config.Admit(ctx, r, false); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	f := stripeMetadata(r, "create")
	f.Set("amount", strconv.FormatInt(r.Amount.Amount, 10))
	f.Set("currency", strings.ToLower(r.Amount.Currency))
	f.Set("payment_method_types[]", "card")
	f.Set("payment_method", s.method)
	var o stripeObject
	if err := s.request(ctx, "POST", "payment_intents", stripeKey(r, "create"), f, &o); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	return stripeResult(o)
}
func (s *Stripe) Capture(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := s.config.Admit(ctx, r.CreatePaymentIntent, true); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	o, err := s.intent(ctx, r)
	if err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if o.Amount != r.Amount.Amount {
		return payment.PaymentIntentResult{}, stripeError(payment.ErrorAmountMismatch)
	}
	if o.Status != "succeeded" {
		if err = s.request(ctx, "POST", "payment_intents/"+r.ProviderRef+"/confirm", stripeKey(r.CreatePaymentIntent, "capture"), url.Values{"payment_method": {s.method}}, &o); err != nil {
			return payment.PaymentIntentResult{}, err
		}
	}
	return stripeResult(o)
}
func (s *Stripe) Cancel(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	o, err := s.intent(ctx, r)
	if err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if o.Amount != r.Amount.Amount {
		return payment.PaymentIntentResult{}, stripeError(payment.ErrorAmountMismatch)
	}
	if o.Status != "canceled" {
		if err = s.request(ctx, "POST", "payment_intents/"+r.ProviderRef+"/cancel", stripeKey(r.CreatePaymentIntent, "cancel"), nil, &o); err != nil {
			return payment.PaymentIntentResult{}, err
		}
	}
	return stripeResult(o)
}
func (s *Stripe) Refund(ctx context.Context, r payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	o, err := s.intent(ctx, r)
	if err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if o.Status != "succeeded" || r.Amount.Amount > o.Amount {
		return payment.PaymentIntentResult{}, stripeError(payment.ErrorStateConflict)
	}
	key := stripeKey(r.CreatePaymentIntent, "refund")
	// Stripe prunes API keys after 24h. Refund metadata is durable: recover the
	// original operation before retrying, and fail closed on a truncated history.
	var list struct {
		Data []stripeObject `json:"data"`
		More bool           `json:"has_more"`
	}
	if err = s.request(ctx, "GET", "refunds?payment_intent="+r.ProviderRef+"&limit=100", "", nil, &list); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	found := false
	for _, v := range list.Data {
		if v.Metadata["osc_key"] == key {
			if v.Amount != r.Amount.Amount || v.Status != "succeeded" {
				return payment.PaymentIntentResult{}, stripeError(payment.ErrorStateConflict)
			}
			found = true
		}
	}
	if !found {
		if list.More {
			return payment.PaymentIntentResult{}, stripeError(payment.ErrorRetryable)
		}
		f := stripeMetadata(r.CreatePaymentIntent, "refund")
		f.Set("payment_intent", r.ProviderRef)
		f.Set("amount", strconv.FormatInt(r.Amount.Amount, 10))
		var refund stripeObject
		if err = s.request(ctx, "POST", "refunds", key, f, &refund); err != nil {
			return payment.PaymentIntentResult{}, err
		}
		if refund.Status != "succeeded" {
			return payment.PaymentIntentResult{}, stripeError(payment.ErrorRetryable)
		}
	}
	o, err = s.intent(ctx, r)
	if err != nil {
		return payment.PaymentIntentResult{}, err
	}
	var charge struct {
		AmountRefunded int64 `json:"amount_refunded"`
	}
	if json.Unmarshal(o.LatestCharge, &charge) != nil {
		return payment.PaymentIntentResult{}, stripeError(payment.ErrorRetryable)
	}
	status := payment.StatusPartiallyRefunded
	if charge.AmountRefunded == o.Amount {
		status = payment.StatusRefunded
	}
	return payment.PaymentIntentResult{ProviderRef: r.ProviderRef, Status: status}, nil
}
