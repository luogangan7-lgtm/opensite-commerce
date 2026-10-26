//go:build pgx

// Admission unit tests make the worker's service boundary observable; real PG
// tests separately prove grants, transactions and restart recovery.
package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type countingExpirer struct {
	calls, sweeps int
	err           error
}

func (s *countingExpirer) Expire(context.Context, string, int) error { s.calls++; return s.err }
func (s *countingExpirer) Sweep(context.Context, string) error       { s.sweeps++; return nil }
func TestWorkerValidatesBeforeService(t *testing.T) {
	s, _ := NewSigner([]byte("disposable-test-signing-key-32bytes"))
	a, _ := s.inventoryArgs("11111111-1111-4111-8111-111111111111", 100)
	service := &countingExpirer{}
	w := &InventoryWorker{Signer: s, Service: service, Sweeper: service}
	for _, kind := range []string{"unknown", InventoryExpireKind} {
		args := a
		if kind == InventoryExpireKind {
			args.TenantID = "22222222-2222-4222-8222-222222222222"
		}
		if err := w.Work(context.Background(), &river.Job[InventoryExpireArgs]{JobRow: &rivertype.JobRow{Kind: kind}, Args: args}); err == nil {
			t.Fatal("untrusted job admitted")
		}
	}
	if service.calls != 0 || service.sweeps != 0 {
		t.Fatal("untrusted job reached service")
	}
	job := &river.Job[InventoryExpireArgs]{JobRow: &rivertype.JobRow{Kind: InventoryExpireKind}, Args: a}
	for range 2 {
		if err := w.Work(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if service.calls != 2 || service.sweeps != 2 {
		t.Fatal("valid job must reach idempotent domain service")
	}
	service.err = errors.New("inventory unavailable")
	if err := w.Work(context.Background(), job); !errors.Is(err, service.err) || service.sweeps != 3 {
		t.Fatal("inventory error suppressed privacy reclamation", err)
	}
}
