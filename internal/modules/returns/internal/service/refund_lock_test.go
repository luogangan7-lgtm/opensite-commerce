// Responsibilities: prove refund finalization lock order and failure recovery.
// Dependencies: shared refund fixtures and public contracts; caller: Go gate.
// Invariants: lock after PSP success, before journal, completion and response.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

func TestRefundLocksPreparedBeforeJournalAndCompletion(t *testing.T) {
	s, r, p, _, _, l, runner := refundFixture(t)
	var events []string
	record := func(event string) {
		events = append(events, event)
		if !runner.active || runner.commits != 1 || len(p.Events()) != 3 {
			t.Errorf("%s must run in finalization after committed prepare and external PSP success: active=%v commits=%d provider=%d", event, runner.active, runner.commits, len(p.Events()))
		}
		if event == "ledger" && r.locks != 1 {
			t.Errorf("ledger posted before order/RMA lock: locks=%d", r.locks)
		}
	}
	r.record, l.record = record, record
	responseCalls := 0
	out, err := s.RefundWithCompletion(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "lock-before-journal"}, func(_ context.Context, _ pgx.Tx, out contract.Return) error {
		responseCalls++
		record("response")
		if out.Status != "refunded" {
			t.Errorf("response persisted before completed refund: %+v", out)
		}
		return nil
	})
	if err != nil || out.Status != "refunded" {
		t.Fatalf("refund finalization: %+v %v", out, err)
	}
	if got := strings.Join(events, ","); got != "lock,ledger,complete,response" {
		t.Errorf("finalization order = %s, want lock,ledger,complete,response", got)
	}
	if r.locks != 1 || l.calls != 1 || r.completes != 1 || responseCalls != 1 || runner.commits != 2 {
		t.Errorf("finalization effects: locks=%d ledger=%d complete=%d response=%d commits=%d", r.locks, l.calls, r.completes, responseCalls, runner.commits)
	}
}

func TestRefundLockFailurePreservesPendingWithoutFinalization(t *testing.T) {
	for name, lockErr := range map[string]error{"unavailable": contract.ErrUnavailable, "retry": contract.ErrRetry} {
		t.Run(name, func(t *testing.T) {
			s, r, p, _, _, l, runner := refundFixture(t)
			r.lockErr = lockErr
			responseCalls := 0
			out, err := s.RefundWithCompletion(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "lock-failure"}, func(context.Context, pgx.Tx, contract.Return) error {
				responseCalls++
				return nil
			})
			if !errors.Is(err, lockErr) || out.ID != "" {
				t.Errorf("lock failure must surface without a receipt: out=%+v err=%v want=%v", out, err, lockErr)
			}
			if r.locks != 1 || l.calls != 0 || r.completes != 0 || responseCalls != 0 || runner.commits != 1 {
				t.Errorf("lock failure finalized effects: locks=%d ledger=%d complete=%d response=%d commits=%d", r.locks, l.calls, r.completes, responseCalls, runner.commits)
			}
			if !r.pending || r.completed || r.prepares != 1 || r.prepared.Status != "refund_pending" || len(p.Events()) != 3 || len(l.posted.Entries) != 0 {
				t.Errorf("lock failure lost durable intent/provider success: pending=%v completed=%v prepares=%d status=%s provider=%d journal=%+v", r.pending, r.completed, r.prepares, r.prepared.Status, len(p.Events()), l.posted)
			}
		})
	}
}
