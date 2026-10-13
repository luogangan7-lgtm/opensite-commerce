// Responsibility: worker send state-machine gate; repository simulates only the
// transaction seam; real SQL/lease semantics belong to pgx integration tests.
package service

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"testing"
	"time"
)

type stateRepo struct {
	row      Envelope
	outcome  string
	begins   int
	finishes int
	accepted bool
}

func (r *stateRepo) Begin(_ context.Context, _ outbox.Lease, _ string) (Envelope, error) {
	r.begins++
	return r.row, nil
}
func (r *stateRepo) Finish(_ context.Context, _ outbox.Lease, _ Envelope, outcome, code string) (bool, error) {
	r.finishes++
	r.outcome = outcome
	return r.accepted, nil
}
func lease() outbox.Lease {
	return outbox.Lease{Delivery: outbox.Delivery{TenantID: "11111111-1111-1111-1111-111111111111", EventID: "22222222-2222-2222-2222-222222222222", Target: "notifications.email", EventType: "notification.dispatch", Payload: []byte(`{"notification_id":"33333333-3333-3333-3333-333333333333"}`)}, WorkerID: "44444444-4444-4444-4444-444444444444", Attempt: 1, LeaseUntil: time.Now().Add(time.Minute)}
}
func TestWorkerReplaysTerminalWithoutSend(t *testing.T) {
	for _, status := range []string{"sent", "uncertain", "dead_letter"} {
		f := &Fake{}
		r := &stateRepo{row: Envelope{Status: status}, accepted: true}
		w := Worker{Repo: r, Sender: f}
		if e := w.Process(context.Background(), lease()); e != nil {
			t.Fatal(e)
		}
		if len(f.Snapshot()) != 0 || r.finishes != 0 {
			t.Fatal("NOTIFICATION_REPLAY_SENT", status)
		}
	}
}
func TestWorkerSuccessFailureUncertainAndFence(t *testing.T) {
	for _, scenario := range []string{"sent", "failed", "uncertain", "fence"} {
		f := &Fake{}
		if scenario == "failed" {
			f.FailNext(1)
		}
		if scenario == "uncertain" {
			f.AmbiguousNext()
		}
		r := &stateRepo{row: Envelope{ID: "33333333-3333-3333-3333-333333333333", EventID: "22222222-2222-2222-2222-222222222222", Recipient: "a@safe.test", Locale: "en", TemplateKey: "order.paid", Reference: "ref", Status: "sending", Token: "token"}, accepted: scenario != "fence"}
		w := Worker{Repo: r, Sender: f}
		e := w.Process(context.Background(), lease())
		if scenario == "failed" && e == nil {
			t.Fatal("NOTIFICATION_KNOWN_FAILURE")
		}
		if scenario == "fence" && !errors.Is(e, outbox.ErrLeaseLost) {
			t.Fatal("NOTIFICATION_FINISH_FENCE", e)
		}
		if (scenario == "sent" || scenario == "uncertain") && e != nil {
			t.Fatal(e)
		}
		want := scenario
		if scenario == "fence" {
			want = "sent"
		}
		if r.outcome != want {
			t.Fatal("NOTIFICATION_OUTCOME", r.outcome, want)
		}
	}
}
func TestWorkerRejectsUntrustedPayload(t *testing.T) {
	r := &stateRepo{}
	w := Worker{Repo: r, Sender: &Fake{}}
	l := lease()
	l.Delivery.Payload = []byte(`{"notification_id":"33333333-3333-3333-3333-333333333333","recipient":"real@example.com"}`)
	if w.Process(context.Background(), l) == nil || r.begins != 0 {
		t.Fatal("NOTIFICATION_PAYLOAD_RECIPIENT")
	}
}
