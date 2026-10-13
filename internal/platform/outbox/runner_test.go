// Responsibility: worker poll/discovery/cancellation gates without claiming SQL proof.
// Dependencies: Dispatcher public contract and stdlib tests; called by Go gates.
// Invariants: no unvalidated tenant claim, no false acknowledgement or raw errors.
package outbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type tenantSourceFunc func(context.Context, string, int32) ([]string, error)

func (f tenantSourceFunc) ActiveTenants(ctx context.Context, after string, limit int32) ([]string, error) {
	return f(ctx, after, limit)
}

type runnerStore struct {
	claimed     []string
	complete    bool
	completeErr error
	claims      int
	acks        int
}

func (s *runnerStore) Claim(_ context.Context, tenantID, workerID string, _, _ int32) ([]Lease, error) {
	s.claimed = append(s.claimed, tenantID)
	s.claims++
	l := fixture(1)
	l.Delivery.TenantID, l.WorkerID = tenantID, workerID
	l.LeaseUntil = time.Now().Add(time.Minute)
	return []Lease{l}, nil
}
func (s *runnerStore) Complete(context.Context, Lease) (bool, error) {
	s.acks++
	return s.complete, s.completeErr
}
func (s *runnerStore) Fail(context.Context, Lease, string, int32) (bool, error) { return true, nil }

func runnerFixture(source TenantSource, store Store, sender Sender) Runner {
	return Runner{Source: source, Dispatcher: Dispatcher{Store: store, Sender: sender, Config: Config{WorkerID: worker, LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 8}}, PollInterval: time.Millisecond, CycleTimeout: time.Second, TenantBatchSize: 1, EventsPerTenant: 1}
}

func TestRunnerDiscoversNewTenantsAndKeepsPageCursor(t *testing.T) {
	second := "44444444-4444-4444-8444-444444444444"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cursors []string
	source := tenantSourceFunc(func(_ context.Context, after string, limit int32) ([]string, error) {
		if limit != 1 {
			t.Fatalf("unbounded page %d", limit)
		}
		cursors = append(cursors, after)
		switch len(cursors) {
		case 1:
			return []string{tenant}, nil
		case 2:
			return nil, nil
		case 3:
			return []string{second}, nil // Added since first traversal.
		default:
			cancel()
			return nil, nil
		}
	})
	store := &runnerStore{complete: true}
	r := runnerFixture(source, store, senderFunc(func(context.Context, Delivery) error { return nil }))
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.claimed, []string{tenant, second}) || !reflect.DeepEqual(cursors, []string{"", tenant, "", second}) {
		t.Fatalf("discovery/continuation mismatch tenants=%v cursors=%v", store.claimed, cursors)
	}
}

func TestRunnerRejectsUntrustedDiscoveryBeforeAnyClaim(t *testing.T) {
	for _, ids := range [][]string{{"invalid"}, {tenant, tenant}, {worker, tenant}, {tenant, worker}} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		store := &runnerStore{complete: true}
		r := runnerFixture(tenantSourceFunc(func(context.Context, string, int32) ([]string, error) { return ids, nil }), store, senderFunc(func(context.Context, Delivery) error { return nil }))
		if len(ids) > 1 {
			r.TenantBatchSize = 3
		}
		if reflect.DeepEqual(ids, []string{tenant, worker}) {
			r.TenantBatchSize = 1
		} // Oversized SQL page.
		if err := r.Run(ctx); err != ErrInvalidInput || store.claims != 0 {
			t.Fatalf("ids=%v err=%v claims=%d", ids, err, store.claims)
		}
		cancel()
	}
}

func TestRunnerStopsActiveSendOnCancellationWithoutAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &runnerStore{complete: true}
	r := runnerFixture(tenantSourceFunc(func(context.Context, string, int32) ([]string, error) { return []string{tenant}, nil }), store, senderFunc(func(ctx context.Context, _ Delivery) error { cancel(); <-ctx.Done(); return ctx.Err() }))
	if err := r.Run(ctx); err != nil || store.claims != 1 || store.acks != 0 {
		t.Fatalf("cancellation err=%v claims=%d acks=%d", err, store.claims, store.acks)
	}
}

func TestRunnerErrorsAreSafeAndNeverAckFalse(t *testing.T) {
	for _, tc := range []struct {
		name              string
		sourceErr, ackErr error
		want              error
	}{
		{"source", errors.New("secret database URL"), nil, ErrTenantDiscovery},
		{"ackfalse", nil, nil, ErrDispatch},
		{"ackerror", nil, errors.New("secret network body"), ErrDispatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			store := &runnerStore{completeErr: tc.ackErr}
			r := runnerFixture(tenantSourceFunc(func(context.Context, string, int32) ([]string, error) { return []string{tenant}, tc.sourceErr }), store, senderFunc(func(context.Context, Delivery) error { return nil }))
			if err := r.Run(ctx); err != tc.want {
				t.Fatalf("safe error = %v", err)
			}
		})
	}
}

func TestRunnerCancellationAndCycleDeadline(t *testing.T) {
	for _, externalCancel := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		source := tenantSourceFunc(func(ctx context.Context, _ string, _ int32) ([]string, error) {
			if externalCancel {
				cancel()
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		r := runnerFixture(source, &runnerStore{complete: true}, senderFunc(func(context.Context, Delivery) error { return nil }))
		r.CycleTimeout = 10 * time.Millisecond
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		select {
		case err := <-done:
			if externalCancel && err != nil || !externalCancel && err != ErrTenantDiscovery {
				t.Fatalf("cancel=%v err=%v", externalCancel, err)
			}
		case <-time.After(time.Second):
			t.Fatal("runner did not stop")
		}
		cancel()
	}
}
