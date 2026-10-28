//go:build pgx && stripe_live

// Responsibility: owner-dispatched A-only entry, compiled before exposing secrets.
// Dependencies: disposable DB fixture and probe; caller: live workflow wrapper.
// No raw child output is public; normal CI executes only the fake-tagged build.
package db_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

// Set only by the compile-time offline canary file. Even replacing its fake
// transport with a real one must not make an offline binary owner-admissible.
var stripeLiveOfflineCompiled bool

func stripeLiveRealHTTPTransport() bool {
	transport, real := http.DefaultTransport.(*http.Transport)
	return !stripeLiveOfflineCompiled && real && transport != nil
}

func TestStripeLiveOwnerGate(t *testing.T) {
	if !stripeLiveRealHTTPTransport() {
		t.Fatal("owner entry requires real HTTP transport and no offline build")
	}
	runStripeLiveProbe(t)
}

func TestStripeLiveTransportGuard(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	if stripeLiveRealHTTPTransport() == stripeLiveOfflineCompiled {
		t.Fatal("unexpected build transport admission")
	}
	http.DefaultTransport = &livePGTransport{}
	if stripeLiveRealHTTPTransport() {
		t.Fatal("fake transport admitted")
	}
	var absent *http.Transport
	http.DefaultTransport = absent
	if stripeLiveRealHTTPTransport() {
		t.Fatal("nil transport admitted")
	}
	http.DefaultTransport = &http.Transport{}
	if stripeLiveRealHTTPTransport() == stripeLiveOfflineCompiled {
		t.Fatal("restored transport erased offline build identity")
	}
}

// Shared probe body; offline output canaries use their own test entry, never
// bypass the real-transport assertion in TestStripeLiveOwnerGate.
func runStripeLiveProbe(t *testing.T) {
	t.Helper()
	if os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("OPENSITE_DISPOSABLE_PG") != "stripe-live" {
		t.Fatal("owner-dispatch gate required")
	}
	o := payments.LiveSmokeOptions{Phase: os.Getenv("STRIPE_SMOKE_PHASE"), RunID: os.Getenv("STRIPE_SMOKE_RUN_ID"), Account: os.Getenv("STRIPE_SMOKE_ACCOUNT")}
	f, s := liveSmokeFixture(t, o)
	v, err := s.Execute(f.ctx)
	liveSmokeWitness(t, f)
	if err != nil {
		emitLiveSummary(v)
		t.Fatal("permission probe unresolved; owner inspection required")
	}
	v.Counts = map[string]int{"capture_journals": 0, "refund_journals": 0, "extra_replay_journals": 0}
	emitLiveSummary(v)
}
