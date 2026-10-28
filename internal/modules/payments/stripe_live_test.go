// Responsibility: fake-only A permission probe admission and endpoint regressions.
// Dependencies: fake HTTP and mapped DB clock; caller: normal CI, never Stripe.
// Invariants: no payment method, no financial endpoints, durable identity refusal.
package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type liveFake struct {
	s        *StripeLiveSmoke
	requests []string
	existing bool
	corrupt  string
}

func newLiveFake(t *testing.T) *liveFake {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("STRIPE_API", "rk_live_offline_fixture")
	c := Config{Run: func(ctx context.Context, account string, fn func(context.Context, pgx.Tx, string, time.Time) error) error {
		if account != "acct_offline_canary" {
			return errors.New("wrong mapped account")
		}
		return fn(ctx, nil, "10000000-0000-4000-8000-000000000001", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	}}
	s, err := NewStripeLiveSmoke(c, LiveSmokeOptions{Phase: "A", RunID: "123456", Account: "acct_offline_canary"})
	if err != nil {
		t.Fatal("offline constructor rejected")
	}
	f := &liveFake{s: s}
	s.client.Transport = stripeTransport(f.roundTrip)
	return f
}

func (f *liveFake) roundTrip(r *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.URL.Host != "api.stripe.com" || r.Header.Get("Stripe-Version") != "2026-09-30.endive" || r.Header.Get("Stripe-Account") != "acct_offline_canary" {
		return nil, errors.New("transport authority")
	}
	if f.corrupt == "transport" {
		return nil, errors.New("https://private.example.test owner-canary@example.test")
	}
	_ = r.ParseForm()
	obj := map[string]any{"id": "pi_offline_canary", "status": "requires_payment_method", "livemode": true, "amount": 50, "amount_received": 0, "currency": "usd", "payment_method": nil, "metadata": map[string]string{"osc_smoke": "123456", "osc_phase": "A"}, "private_url": "https://private.example.test", "email": "owner-canary@example.test"}
	var out any = obj
	switch {
	case r.Method == "GET":
		data := []any{}
		if f.existing {
			data = append(data, obj)
		}
		out = map[string]any{"data": data, "has_more": false}
		switch f.corrupt {
		case "missing_list":
			out = map[string]any{"data": data}
		case "empty_page":
			out = map[string]any{"data": data, "has_more": true}
		case "duplicate_page":
			obj["metadata"] = map[string]string{"osc_smoke": "other"}
			out = map[string]any{"data": []any{obj}, "has_more": true}
		}
	case r.URL.Path == "/v1/payment_intents":
		if r.Form.Get("amount") != "50" || r.Form.Get("currency") != "usd" || len(r.Form) != 4 || r.Header.Get("Idempotency-Key") == "" {
			return nil, errors.New("unexpected create form")
		}
		f.existing = true
	case r.URL.Path == "/v1/payment_intents/pi_offline_canary/cancel":
		obj["status"] = "canceled"
		if len(r.Form) != 0 {
			return nil, errors.New("unexpected cancel form")
		}
		if f.corrupt == "cancel_identity" {
			obj["id"] = "pi_another"
		}
		if f.corrupt == "cancel_status" {
			obj["status"] = "processing"
		}
	default:
		return nil, errors.New("forbidden endpoint")
	}
	if r.Method == "POST" {
		switch f.corrupt {
		case "mode":
			obj["livemode"] = false
		case "amount":
			obj["amount"] = 5000
		case "currency":
			obj["currency"] = "eur"
		case "metadata":
			obj["metadata"] = map[string]string{"osc_smoke": "other"}
		case "received":
			obj["amount_received"] = 50
		case "missing_received":
			delete(obj, "amount_received")
		case "method":
			obj["payment_method"] = "pm_private"
		case "missing_method":
			delete(obj, "payment_method")
		case "succeeded":
			obj["status"] = "succeeded"
		}
	}
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}, nil
}

func TestStripeLiveAOnlyLifecycle(t *testing.T) {
	f := newLiveFake(t)
	v, err := f.s.Execute(context.Background())
	if err != nil || v.Status != "CANCELED" || v.AmountMinor != 50 {
		t.Fatal("permission probe failed")
	}
	if strings.Join(f.requests, ",") != "GET /v1/payment_intents,POST /v1/payment_intents,POST /v1/payment_intents/pi_offline_canary/cancel" {
		t.Fatal("unexpected capability usage")
	}
	if _, err := f.s.Execute(context.Background()); err == nil || len(f.requests) != 3 {
		t.Fatal("same instance repeated writes")
	}
	// Discard all local state and idempotency cache, retain only provider history.
	f.s.started = false
	if _, err := f.s.Execute(context.Background()); err == nil || len(f.requests) != 4 {
		t.Fatal("durable run identity admitted another create")
	}
}

func TestStripeLiveAOnlyAdmission(t *testing.T) {
	for _, phase := range []string{"B", "R", "", "AA"} {
		f := newLiveFake(t)
		o := f.s.options
		o.Phase = phase
		if _, err := NewStripeLiveSmoke(f.s.config, o); err == nil {
			t.Errorf("admitted phase %q", phase)
		}
	}
	for name, values := range map[string][]string{
		"STRIPE_API":         {"sk_live_private", "sk_test_private", "rk_test_private", "rk_live_", "rk_live_bad\n"},
		"GITHUB_RUN_ATTEMPT": {"2", "99", "", "0", "bad"},
		"GITHUB_EVENT_NAME":  {"push", "pull_request"},
		"GITHUB_REF":         {"refs/heads/feature"},
		"GITHUB_ACTIONS":     {"false"},
	} {
		for _, value := range values {
			f := newLiveFake(t)
			t.Setenv(name, value)
			if _, err := NewStripeLiveSmoke(f.s.config, f.s.options); err == nil {
				t.Errorf("admitted invalid %s", name)
			}
		}
	}
}

func TestStripeLiveAOnlyFailsClosed(t *testing.T) {
	for _, corrupt := range []string{"transport", "missing_list", "empty_page", "duplicate_page", "mode", "amount", "currency", "metadata", "received", "missing_received", "method", "missing_method", "succeeded", "cancel_identity", "cancel_status"} {
		t.Run(corrupt, func(t *testing.T) {
			f := newLiveFake(t)
			f.corrupt = corrupt
			v, err := f.s.Execute(context.Background())
			if err == nil || v.Status != "PENDING" || strings.Contains(err.Error(), "private.example") || strings.Contains(err.Error(), "@") {
				t.Fatal("invalid provider evidence escaped fail-closed boundary")
			}
		})
	}
}

func TestStripeLiveAOnlyEndpoints(t *testing.T) {
	for _, path := range []string{"refunds", "invoices", "customers", "events", "payment_methods", "payment_intents/pi_probe/confirm", "payment_intents/pi_probe/capture", "payment_intents/pi_probe", "invoices/in_probe/send", "payment_intents?confirm=true", "payment_intents/pi_probe/cancel?x=1", "payment_intents/pi_probe%2fconfirm/cancel"} {
		if liveEndpoint("POST", path) {
			t.Errorf("A-only admitted POST %s", path)
		}
	}
	for _, path := range []string{"payment_intents", "payment_intents?limit=100&expand[]=data", "payment_intents?limit=100&starting_after=bad", "payment_intents?limit=100&limit=100"} {
		if liveEndpoint("GET", path) {
			t.Errorf("A-only admitted GET %s", path)
		}
	}
	for _, path := range []string{"payment_intents", "payment_intents/pi_probe/cancel"} {
		if !liveEndpoint("POST", path) {
			t.Errorf("refused probe endpoint %s", path)
		}
	}
	f := newLiveFake(t)
	for _, field := range []string{"payment_method", "confirm", "capture_method", "customer"} {
		form := url.Values{"amount": {"50"}, "currency": {"usd"}, "metadata[osc_smoke]": {"123456"}, "metadata[osc_phase]": {"A"}, field: {"unsafe"}}
		if f.s.request(context.Background(), "POST", "payment_intents", "key", form, &liveObject{}) == nil || len(f.requests) != 0 {
			t.Fatal("unsafe form reached transport")
		}
	}
}
