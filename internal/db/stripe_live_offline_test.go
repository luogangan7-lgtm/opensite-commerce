//go:build pgx

// Responsibility: real PG zero-journal witness with fake-only Stripe transport.
// Dependencies: A probe and migrated PG fixture; caller: normal Foundation CI.
// No real credential or live network; every non-probe endpoint fails closed.
package db_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

type livePGTransport struct {
	creates, cancels int
	scenario        string
}

func (f *livePGTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if f.scenario == "failure" {
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"https://invoice.example.test/private-canary owner-canary@example.test pi_full_private_identifier"}}`))}, nil
	}
	obj := map[string]any{"id": "pi_offlinepg", "status": "requires_payment_method", "livemode": true, "amount": 50, "amount_received": 0, "currency": "usd", "payment_method": nil, "metadata": map[string]string{"osc_smoke": "246810", "osc_phase": "A"}, "private_url": "https://invoice.example.test/private-canary", "email": "owner-canary@example.test"}
	var out any = obj
	switch {
	case r.URL.Path == "/v1/payment_intents" && r.Method == "GET":
		out = map[string]any{"data": []any{}, "has_more": false}
	case r.URL.Path == "/v1/payment_intents" && r.Method == "POST":
		f.creates++
	case r.URL.Path == "/v1/payment_intents/pi_offlinepg/cancel" && r.Method == "POST":
		f.cancels++
		obj["status"] = "canceled"
	default:
		return nil, errors.New("forbidden live probe endpoint")
	}
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}, nil
}

func TestStripeLiveOfflinePGZeroJournals(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("STRIPE_API", "rk_live_OFFLINE_NOT_A_REAL_CREDENTIAL")
	fake := &livePGTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = fake
	t.Cleanup(func() { http.DefaultTransport = old })
	f, s := liveSmokeFixture(t, payments.LiveSmokeOptions{Phase: "A", RunID: "246810", Account: "acct_offlinepg"})
	liveSmokeWitness(t, f)
	v, err := s.Execute(f.ctx)
	if err != nil || v.Status != "CANCELED" || fake.creates != 1 || fake.cancels != 1 {
		t.Fatal("fake-provider A permission probe failed")
	}
	liveSmokeWitness(t, f)
}
