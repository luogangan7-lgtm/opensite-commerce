//go:build pgx && stripe_live

// Responsibility: owner-dispatched A-only entry, compiled before exposing secrets.
// Dependencies: disposable DB fixture and probe; caller: live workflow wrapper.
// No raw child output is public; normal CI executes only the fake-tagged build.
package db_test

import (
	"os"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
)

func TestStripeLiveOwnerGate(t *testing.T) {
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
