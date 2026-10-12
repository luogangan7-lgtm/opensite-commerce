//go:build pgx

// Admission unit tests make the worker's service boundary observable; real PG
// tests separately prove grants, transactions and restart recovery.
package jobs

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type countingExpirer struct{ calls int }

func (s *countingExpirer) Expire(context.Context, string, int) error { s.calls++; return nil }
func TestWorkerValidatesBeforeService(t *testing.T) {
	s, _ := NewSigner([]byte("disposable-test-signing-key-32bytes"))
	a, _ := s.inventoryArgs("11111111-1111-4111-8111-111111111111", 100)
	service := &countingExpirer{}
	w := &InventoryWorker{Signer: s, Service: service}
	for _, kind := range []string{"unknown", InventoryExpireKind} {
		args := a
		if kind == InventoryExpireKind {
			args.TenantID = "22222222-2222-4222-8222-222222222222"
		}
		if err := w.Work(context.Background(), &river.Job[InventoryExpireArgs]{JobRow: &rivertype.JobRow{Kind: kind}, Args: args}); err == nil {
			t.Fatal("untrusted job admitted")
		}
	}
	if service.calls != 0 {
		t.Fatal("untrusted job reached service")
	}
	job := &river.Job[InventoryExpireArgs]{JobRow: &rivertype.JobRow{Kind: InventoryExpireKind}, Args: a}
	for range 2 {
		if err := w.Work(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if service.calls != 2 {
		t.Fatal("valid job must reach idempotent domain service")
	}
}
