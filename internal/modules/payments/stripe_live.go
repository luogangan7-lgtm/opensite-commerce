// Responsibility: owner-gated A-only restricted-key permission probe.
// Dependencies: authenticated Stripe HTTP and the mapped disposable DB clock.
// Caller: CI-only fixture; production composition is forbidden by the static gate.
// Invariants: no payment method, no financial ingestion, no provider IO under locks.
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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// LiveSmokeOptions contains identity only; credentials remain environment-only.
type LiveSmokeOptions struct {
	Phase   string
	RunID   string
	Account string
}

type LiveSmokeSummary struct {
	Phase             string         `json:"phase"`
	Status            string         `json:"status"`
	AmountMinor       int64          `json:"amount_minor"`
	Currency          string         `json:"currency"`
	Livemode          bool           `json:"livemode"`
	OwnerActionNeeded bool           `json:"owner_action_needed"`
	Counts            map[string]int `json:"counts,omitempty"`
}

// Unknown response fields, including private URLs and emails, are discarded.
type liveObject struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Livemode       bool              `json:"livemode"`
	Amount         int64             `json:"amount"`
	AmountReceived *int64            `json:"amount_received"`
	Currency       string            `json:"currency"`
	PaymentMethod  json.RawMessage   `json:"payment_method"`
	Metadata       map[string]string `json:"metadata"`
}

type StripeLiveSmoke struct {
	config  Config
	options LiveSmokeOptions
	secret  string
	client  *http.Client
	started bool
}

func liveFailure() error {
	return errors.New("live permission probe unresolved; provider details withheld")
}

func liveID(s, prefix string) bool {
	return strings.HasPrefix(s, prefix+"_") && len(s) <= 128 && accountPattern.MatchString(s)
}

func liveRun(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func NewStripeLiveSmoke(c Config, o LiveSmokeOptions) (*StripeLiveSmoke, error) {
	key := os.Getenv("STRIPE_API")
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" ||
		os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" ||
		!strings.HasPrefix(key, "rk_live_") || len(key) <= 8 ||
		strings.ContainsFunc(key, func(r rune) bool { return r < '!' || r > '~' }) ||
		!liveID(o.Account, "acct") || !liveRun(o.RunID) || c.Run == nil || o.Phase != "A" {
		return nil, liveFailure()
	}
	return &StripeLiveSmoke{config: c, options: o, secret: key, client: &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (s *StripeLiveSmoke) now(ctx context.Context) error {
	var at time.Time
	err := s.config.Run(ctx, s.options.Account, func(_ context.Context, _ pgx.Tx, _ string, t time.Time) error {
		at = t
		return nil
	})
	if err != nil || at.IsZero() {
		return liveFailure()
	}
	return nil
}

func (s *StripeLiveSmoke) key(op string) string {
	h := sha256.Sum256([]byte("stripe-live:" + s.options.RunID + ":" + op))
	return hex.EncodeToString(h[:])
}

// This is an application capability bound, not proof of actual Dashboard grants.
func liveEndpoint(method, path string) bool {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	if method == "GET" && u.Path == "payment_intents" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || q.Get("limit") != "100" || len(q["limit"]) != 1 {
			return false
		}
		for key, values := range q {
			if key != "limit" && (key != "starting_after" || len(values) != 1 || !liveID(values[0], "pi")) {
				return false
			}
		}
		return true
	}
	if u.RawQuery != "" || method != "POST" {
		return false
	}
	parts := strings.Split(path, "/")
	return path == "payment_intents" || (len(parts) == 3 && parts[0] == "payment_intents" && liveID(parts[1], "pi") && parts[2] == "cancel")
}

func (s *StripeLiveSmoke) request(ctx context.Context, method, path, key string, form url.Values, out any) error {
	if !liveEndpoint(method, path) {
		return liveFailure()
	}
	if method == "POST" {
		if key == "" {
			return liveFailure()
		}
		if path == "payment_intents" {
			expected := url.Values{"amount": {"50"}, "currency": {"usd"}, "metadata[osc_smoke]": {s.options.RunID}, "metadata[osc_phase]": {"A"}}
			if form.Encode() != expected.Encode() {
				return liveFailure()
			}
		} else if len(form) != 0 {
			return liveFailure()
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com/v1/"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return liveFailure()
	}
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Stripe-Version", "2026-09-30.endive")
	req.Header.Set("Stripe-Account", s.options.Account)
	if method == "POST" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return liveFailure()
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || res.StatusCode < 200 || res.StatusCode >= 300 || json.Unmarshal(b, out) != nil {
		return liveFailure()
	}
	return nil
}

// A fresh adapter checks durable metadata even after provider idempotency expiry.
// Existing or uncertain identities fail closed; no recovery write path exists.
func (s *StripeLiveSmoke) requireFreshIdentity(ctx context.Context) error {
	after := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		var list struct {
			Data []liveObject `json:"data"`
			More *bool        `json:"has_more"`
		}
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("starting_after", after)
		}
		if s.request(ctx, "GET", "payment_intents?"+q.Encode(), "", nil, &list) != nil || list.Data == nil || list.More == nil {
			return liveFailure()
		}
		for _, o := range list.Data {
			if !liveID(o.ID, "pi") || seen[o.ID] || o.Metadata["osc_smoke"] == s.options.RunID {
				return liveFailure()
			}
			seen[o.ID] = true
		}
		if !*list.More {
			return nil
		}
		if len(list.Data) == 0 {
			return liveFailure()
		}
		after = list.Data[len(list.Data)-1].ID
	}
	return liveFailure()
}

func (s *StripeLiveSmoke) validObject(o liveObject) bool {
	return liveID(o.ID, "pi") && o.Livemode && o.Metadata["osc_smoke"] == s.options.RunID &&
		o.Metadata["osc_phase"] == "A" && o.Currency == "usd" && o.Amount == 50 &&
		o.AmountReceived != nil && *o.AmountReceived == 0 && string(o.PaymentMethod) == "null"
}

func (s *StripeLiveSmoke) Execute(ctx context.Context) (LiveSmokeSummary, error) {
	v := LiveSmokeSummary{Phase: "A", Status: "PENDING", AmountMinor: 50, Currency: "USD", Livemode: true}
	if s.started || s.now(ctx) != nil {
		return v, liveFailure()
	}
	s.started = true
	if err := s.requireFreshIdentity(ctx); err != nil {
		return v, err
	}
	form := url.Values{"amount": {"50"}, "currency": {"usd"}, "metadata[osc_smoke]": {s.options.RunID}, "metadata[osc_phase]": {"A"}}
	var pi liveObject
	if s.request(ctx, "POST", "payment_intents", s.key("a-create"), form, &pi) != nil || !s.validObject(pi) || pi.Status != "requires_payment_method" {
		return v, liveFailure()
	}
	ref := pi.ID
	// Cancellation is independent provider evidence; omitted fields must never
	// inherit the successful create response's identity or zero-received state.
	canceled := pi
	if s.request(ctx, "POST", "payment_intents/"+ref+"/cancel", s.key("a-cancel"), nil, &canceled) != nil || canceled.ID != ref || !s.validObject(canceled) || canceled.Status != "canceled" {
		return v, liveFailure()
	}
	v.Status = "CANCELED"
	return v, nil
}
