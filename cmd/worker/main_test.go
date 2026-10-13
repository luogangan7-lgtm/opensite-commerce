//go:build pgx

// Responsibility: actual startup fail-closed and disposable PG composition gates.
// Dependencies: main runContext seam and stdlib; called by worker Go gates.
// Invariant: no Sender configuration means no database claim or false send.
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestStartupEnabledWithoutSenderFailsBeforeDatabaseAccess(t *testing.T) {
	for _, settings := range []map[string]string{{"OUTBOX_ENABLED": "true"}, {"OUTBOX_ENABLED": "true", "WORKER_DATABASE_URL": "invalid", "OUTBOX_DATABASE_URL": "invalid", "JOB_SIGNING_KEY": "secret"}} {
		if err := runContext(context.Background(), env(settings), nil, composeWorkers); err == nil || err.Error() != "OUTBOX_SENDER_NOT_CONFIGURED" {
			t.Fatalf("startup error=%v", err)
		}
	}
}

func TestStartupRejectsInvalidOutboxModeAndDefaultsDisabled(t *testing.T) {
	if err := runContext(context.Background(), env(map[string]string{"OUTBOX_ENABLED": "yes"}), nil, composeWorkers); err == nil || err.Error() != "invalid OUTBOX_ENABLED" {
		t.Fatalf("mode error=%v", err)
	}
	if err := runContext(context.Background(), env(nil), nil, composeWorkers); err == nil || err.Error() != "WORKER_DATABASE_URL required" {
		t.Fatalf("default disabled error=%v", err)
	}
}

type testJobs struct {
	started, stopped  bool
	startErr, stopErr error
	startCtx          context.Context
	canceledAtStop    bool
}

func (w *testJobs) Start(ctx context.Context) error {
	w.started, w.startCtx = true, ctx
	return w.startErr
}
func (w *testJobs) Stop(context.Context) error {
	w.stopped = true
	w.canceledAtStop = w.startCtx != nil && w.startCtx.Err() != nil
	return w.stopErr
}

// River treats a canceled Start context as StopAndCancel; a signal must reach
// running jobs only through Stop's grace window (PR25 review, main.go).
func TestSignalDoesNotCancelJobsBeforeGracefulStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := &testJobs{}
	factory := func(context.Context, func(string) string, outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
		return jobs, nil, func() {}, nil
	}
	cancel()
	if err := runContext(ctx, env(nil), nil, factory); err != nil {
		t.Fatal(err)
	}
	if !jobs.stopped || jobs.canceledAtStop {
		t.Fatalf("jobs context canceled before Stop: %+v", jobs)
	}
}

type testSender func(context.Context, outbox.Delivery) error

func (f testSender) Send(ctx context.Context, d outbox.Delivery) error { return f(ctx, d) }

type testSource func(context.Context, string, int32) ([]string, error)

func (f testSource) ActiveTenants(ctx context.Context, a string, n int32) ([]string, error) {
	return f(ctx, a, n)
}

type startupStore struct {
	ackOK   bool
	ack     int
	claimed int
}

func (s *startupStore) Claim(_ context.Context, t, w string, _, _ int32) ([]outbox.Lease, error) {
	s.claimed++
	return []outbox.Lease{{Delivery: outbox.Delivery{TenantID: t, EventID: "33333333-3333-4333-8333-333333333333", Target: "test:consumer", DedupKey: "startup"}, WorkerID: w, Attempt: 1, LeaseUntil: time.Now().Add(time.Minute)}}, nil
}
func (s *startupStore) Complete(context.Context, outbox.Lease) (bool, error) {
	s.ack++
	return s.ackOK, nil
}
func (s *startupStore) Fail(context.Context, outbox.Lease, string, int32) (bool, error) {
	return true, nil
}

func TestActualStartupRunsDispatcherAndStopsBothWorkers(t *testing.T) {
	for _, ackOK := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		jobs := &testJobs{}
		store := &startupStore{ackOK: ackOK}
		cleaned := false
		sender := testSender(func(context.Context, outbox.Delivery) error { return nil })
		factory := func(_ context.Context, _ func(string) string, s outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
			r := &outbox.Runner{Source: testSource(func(_ context.Context, after string, _ int32) ([]string, error) {
				if !jobs.started {
					t.Error("dispatcher ran before jobs startup")
				}
				if after != "" {
					cancel()
					return nil, nil
				}
				return []string{"11111111-1111-4111-8111-111111111111"}, nil
			}), Dispatcher: outbox.Dispatcher{Store: store, Sender: s, Config: outbox.Config{WorkerID: "22222222-2222-4222-8222-222222222222", LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 8}}, TenantBatchSize: 1, EventsPerTenant: 1, PollInterval: time.Millisecond, CycleTimeout: time.Second}
			return jobs, r, func() { cleaned = true }, nil
		}
		err := runContext(ctx, env(map[string]string{"OUTBOX_ENABLED": "true", "OUTBOX_DATABASE_URL": "test-only"}), sender, factory)
		cancel()
		if ackOK && err != nil || !ackOK && !errors.Is(err, outbox.ErrDispatch) {
			t.Fatalf("ack=%v error=%v", ackOK, err)
		}
		if !jobs.started || !jobs.stopped || !cleaned || store.claimed != 1 || store.ack != 1 {
			t.Fatalf("lifecycle jobs=%+v cleaned=%v claims=%d ack=%d", jobs, cleaned, store.claimed, store.ack)
		}
	}
}

func TestStartupJobsErrorStopsBeforeDispatcher(t *testing.T) {
	jobs := &testJobs{startErr: errors.New("secret database details")}
	cleaned := false
	factory := func(context.Context, func(string) string, outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
		return jobs, nil, func() { cleaned = true }, nil
	}
	if err := runContext(context.Background(), env(nil), nil, factory); err == nil || err.Error() != "worker startup failed" || !cleaned || jobs.stopped {
		t.Fatalf("startup error=%v cleaned=%v stopped=%v", err, cleaned, jobs.stopped)
	}
}
