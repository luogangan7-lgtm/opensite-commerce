// Responsibility: dispatcher state and transaction ordering gates, without PG.
// Dependencies: testing/stdlib; real SQL fencing is separately tested with pgx.
// Called by check_all; failures must preserve leases for later takeover.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const worker = "22222222-2222-4222-8222-222222222222"
const event = "33333333-3333-4333-8333-333333333333"

var fixtureExpiry = time.Now().Add(time.Minute)

type senderFunc func(context.Context, Delivery) error

func (f senderFunc) Send(ctx context.Context, e Delivery) error { return f(ctx, e) }

type fakeStore struct {
	queue                                []Lease
	calls                                []string
	completeOK, failOK                   bool
	claimError, completeError, failError error
	ack                                  Lease
	failureCode                          string
	retry                                int32
}

func (s *fakeStore) Claim(_ context.Context, t, w string, limit, seconds int32) ([]Lease, error) {
	s.calls = append(s.calls, "claim-commit")
	if t != tenant || w != worker || limit != 1 || seconds != 60 {
		panic("unexpected claim identity or shape")
	}
	if s.claimError != nil {
		return nil, s.claimError
	}
	if len(s.queue) == 0 {
		return nil, nil
	}
	e := s.queue[0]
	s.queue = s.queue[1:]
	return []Lease{e}, nil
}
func (s *fakeStore) Complete(_ context.Context, l Lease) (bool, error) {
	s.calls = append(s.calls, "ack-commit")
	s.ack = l
	return s.completeOK, s.completeError
}
func (s *fakeStore) Fail(_ context.Context, l Lease, code string, retry int32) (bool, error) {
	s.calls = append(s.calls, "fail-commit")
	s.ack = l
	s.failureCode = code
	s.retry = retry
	return s.failOK, s.failError
}
func fixture(attempt int32) Lease {
	return Lease{Delivery: Delivery{TenantID: tenant, EventID: event, Target: "test:consumer", DedupKey: "stable-key", EventType: "test", Payload: []byte(`{"x":1}`)}, WorkerID: worker, Attempt: attempt, LeaseUntil: fixtureExpiry}
}
func dispatcher(s *fakeStore, f senderFunc) Dispatcher {
	return Dispatcher{Store: s, Sender: f, Config: Config{WorkerID: worker, LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 10}}
}

func TestClaimCommitsBeforeSendAndAckUsesSameFence(t *testing.T) {
	s := &fakeStore{queue: []Lease{fixture(7)}, completeOK: true}
	d := dispatcher(s, func(ctx context.Context, e Delivery) error {
		s.calls = append(s.calls, "send")
		if deadline, ok := ctx.Deadline(); !ok || deadline.After(fixtureExpiry) {
			t.Fatal("sender deadline exceeds lease")
		}
		if !reflect.DeepEqual(e, fixture(7).Delivery) {
			t.Fatalf("identity drift: %+v", e)
		}
		return nil
	})
	r, err := d.Dispatch(context.Background(), tenant, 1)
	if err != nil || r != (Result{Claimed: 1, Sent: 1, Completed: 1}) {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	if !reflect.DeepEqual(s.calls, []string{"claim-commit", "send", "ack-commit"}) || !reflect.DeepEqual(s.ack, fixture(7)) {
		t.Fatalf("order=%v fence=%+v", s.calls, s.ack)
	}
}

func TestSendFailurePersistsSafeBoundedExponentialRetry(t *testing.T) {
	for _, tc := range []struct{ attempt, seconds int32 }{{1, 2}, {2, 4}, {3, 8}, {4, 10}, {2147483647, 10}} {
		t.Run(fmt.Sprint(tc.attempt), func(t *testing.T) {
			s := &fakeStore{queue: []Lease{fixture(tc.attempt)}, failOK: true}
			d := dispatcher(s, func(context.Context, Delivery) error {
				s.calls = append(s.calls, "send")
				return errors.New("secret URL body")
			})
			r, err := d.Dispatch(context.Background(), tenant, 1)
			if err != nil || r != (Result{Claimed: 1, Failed: 1}) || s.retry != tc.seconds || s.failureCode != "OUTBOX_SEND_FAILED" {
				t.Fatalf("report=%+v retry=%d code=%s err=%v", r, s.retry, s.failureCode, err)
			}
			if !reflect.DeepEqual(s.calls, []string{"claim-commit", "send", "fail-commit"}) || s.ack.Attempt != tc.attempt {
				t.Fatalf("order/fence %+v", s)
			}
		})
	}
}

func TestExpiredClaimNeverSendsAndEmptyPollIsSuccessful(t *testing.T) {
	l := fixture(1)
	l.LeaseUntil = time.Now().Add(-time.Second)
	s := &fakeStore{queue: []Lease{l}}
	d := dispatcher(s, func(context.Context, Delivery) error { t.Fatal("expired lease sent"); return nil })
	r, err := d.Dispatch(context.Background(), tenant, 1)
	if !errors.Is(err, ErrLeaseLost) || r.Claimed != 1 || len(s.calls) != 1 {
		t.Fatalf("%+v %v %v", r, err, s.calls)
	}
	r, err = d.Dispatch(context.Background(), tenant, 1)
	if err != nil || r != (Result{}) {
		t.Fatalf("empty poll %+v %v", r, err)
	}
}

func TestFailureCommitErrorStopsWithoutClaimingMore(t *testing.T) {
	x := errors.New("fail commit failed")
	s := &fakeStore{queue: []Lease{fixture(1), fixture(2)}, failOK: true, failError: x}
	d := dispatcher(s, func(context.Context, Delivery) error { return errors.New("send failed") })
	r, err := d.Dispatch(context.Background(), tenant, 2)
	if !errors.Is(err, x) || r.Failed != 0 || r.Claimed != 1 || len(s.calls) != 2 {
		t.Fatalf("%+v %v %v", r, err, s.calls)
	}
}

func TestFalseAckAndFalseFailNeverReportCompletion(t *testing.T) {
	for _, sendFails := range []bool{false, true} {
		s := &fakeStore{queue: []Lease{fixture(1)}}
		d := dispatcher(s, func(context.Context, Delivery) error {
			if sendFails {
				return errors.New("failed")
			}
			return nil
		})
		r, err := d.Dispatch(context.Background(), tenant, 2)
		if !errors.Is(err, ErrLeaseLost) || r.Completed != 0 || r.Failed != 0 || r.Claimed != 1 {
			t.Fatalf("stale lease accepted %+v %v", r, err)
		}
	}
}

func TestAckCrashStopsWithoutFailingOrResending(t *testing.T) {
	errCrash := errors.New("ack transport ambiguous")
	s := &fakeStore{queue: []Lease{fixture(1), fixture(2)}, completeOK: true, completeError: errCrash}
	sent := 0
	d := dispatcher(s, func(context.Context, Delivery) error { sent++; return nil })
	r, err := d.Dispatch(context.Background(), tenant, 2)
	if !errors.Is(err, errCrash) || sent != 1 || r.Completed != 0 || len(s.calls) != 2 {
		t.Fatalf("ambiguous ack mishandled %+v %v calls=%v", r, err, s.calls)
	}
}

func TestClaimErrorNeverSends(t *testing.T) {
	x := errors.New("claim commit failed")
	s := &fakeStore{claimError: x}
	d := dispatcher(s, func(context.Context, Delivery) error { t.Fatal("send before commit"); return nil })
	if _, err := d.Dispatch(context.Background(), tenant, 1); !errors.Is(err, x) {
		t.Fatal(err)
	}
}

func TestUntrustedClaimIdentityNeverSends(t *testing.T) {
	for _, change := range []func(*Lease){func(l *Lease) { l.Delivery.TenantID = worker }, func(l *Lease) { l.WorkerID = tenant }, func(l *Lease) { l.Attempt = 0 }, func(l *Lease) { l.Delivery.EventID = "invalid" }, func(l *Lease) { l.LeaseUntil = time.Time{} }} {
		l := fixture(1)
		change(&l)
		s := &fakeStore{queue: []Lease{l}}
		d := dispatcher(s, func(context.Context, Delivery) error { t.Fatal("invalid lease sent"); return nil })
		if _, err := d.Dispatch(context.Background(), tenant, 1); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
}

func TestValidationAndCancellationBeforeClaim(t *testing.T) {
	for _, change := range []func(*Dispatcher){func(d *Dispatcher) { d.Config.WorkerID = "bad" }, func(d *Dispatcher) { d.Config.LeaseSeconds = 0 }, func(d *Dispatcher) { d.Config.BaseRetrySeconds = 0 }, func(d *Dispatcher) { d.Config.MaxRetrySeconds = 86401 }, func(d *Dispatcher) { d.Store = nil }, func(d *Dispatcher) { d.Sender = nil }} {
		s := &fakeStore{}
		d := dispatcher(s, func(context.Context, Delivery) error { return nil })
		change(&d)
		if _, err := d.Dispatch(context.Background(), tenant, 1); !errors.Is(err, ErrInvalidInput) || len(s.calls) != 0 {
			t.Fatalf("%v %v", err, s.calls)
		}
	}
	s := &fakeStore{}
	d := dispatcher(s, func(context.Context, Delivery) error { return nil })
	for _, id := range []string{"bad", "00000000-0000-0000-0000-000000000000"} {
		if _, err := d.Dispatch(context.Background(), id, 1); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, err := d.Dispatch(context.Background(), tenant, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Dispatch(ctx, tenant, 1); !errors.Is(err, context.Canceled) || len(s.calls) != 0 {
		t.Fatalf("%v %v", err, s.calls)
	}
}
