//go:build pgx

// Responsibility: worker component credentials and controlled metric query tests.
// Dependencies: worker helpers and stdlib; called by pgx/race Go gates.
// Invariants: no secret appears in errors; only the controlled aggregate SQL is read.
package main

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
)

func TestWorkerDatabaseComponents(t *testing.T) {
	secret := "punctuation:+@%/a-long-private-password"
	settings := map[string]string{"OPENSITE_DB_HOST": "postgres", "OPENSITE_DB": "opensite", "OPENSITE_WORKER_DB_PASSWORD": secret}
	dsn, err := workerDSN(env(settings), "WORKER_DATABASE_URL", "WORKER", "opensite_queue_login")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if parsed.User.Username() != "opensite_queue_login" || password != secret || parsed.Host != "postgres:5432" || parsed.Path != "/opensite" || parsed.Query().Get("sslmode") != "disable" {
		t.Fatal("credential URL changed capability or punctuation")
	}
	settings["WORKER_DATABASE_URL"] = "explicit"
	if got, err := workerDSN(env(settings), "WORKER_DATABASE_URL", "WORKER", "opensite_queue_login"); err != nil || got != "explicit" {
		t.Fatal("explicit DSN was not preserved")
	}
	delete(settings, "WORKER_DATABASE_URL")
	for _, key := range []string{"OPENSITE_DB_HOST", "OPENSITE_DB", "OPENSITE_WORKER_DB_PASSWORD"} {
		old := settings[key]
		settings[key] = "invalid:?secret"
		if _, err := workerDSN(env(settings), "WORKER_DATABASE_URL", "WORKER", "opensite_queue_login"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid %s leaked or passed: %v", key, err)
		}
		settings[key] = old
	}
	if got, err := workerDSN(env(nil), "WORKER_DATABASE_URL", "WORKER", "opensite_queue_login"); err != nil || got != "" {
		t.Fatal("missing configuration should preserve required-field check")
	}
}

type metricRow struct {
	pending, dead int64
	age           float64
	err           error
}

func (r metricRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64), *dest[1].(*int64), *dest[2].(*float64) = r.pending, r.dead, r.age
	return nil
}

type metricQuery struct {
	row   metricRow
	query string
}

func (q *metricQuery) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	q.query = query
	return q.row
}
func TestControlledWorkerMetricSnapshot(t *testing.T) {
	query := &metricQuery{row: metricRow{pending: 3, dead: 2, age: 1.5}}
	snapshot, err := (outboxMetrics{query: query}).Snapshot(context.Background())
	if err != nil || snapshot.Pending != 3 || snapshot.DeadLetters != 2 || snapshot.OldestPendingAge != 1500*time.Millisecond {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
	if query.query != "SELECT pending, dead_letters, oldest_pending_age_seconds FROM app.outbox_metrics()" {
		t.Fatalf("uncontrolled query=%s", query.query)
	}
	for _, row := range []metricRow{{pending: -1}, {dead: -1}, {age: -1}, {age: math.NaN()}, {age: math.Inf(1)}, {err: errors.New("private database detail")}} {
		query.row = row
		if _, err := (outboxMetrics{query: query}).Snapshot(context.Background()); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid aggregate should fail without details: %v", err)
		}
	}
	if _, err := (outboxMetrics{}).Snapshot(context.Background()); err == nil {
		t.Fatal("missing pool must not export zeros")
	}
}

// The listener uses the same startup path as the executable and retains no
// goroutine after graceful stop; no polling/sleep orders the lifecycle test.
func TestWorkerMonitorHealthAndGracefulStop(t *testing.T) {
	monitor, err := startWorkerMonitor("127.0.0.1:0", fixedMetricSource{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = monitor.server.Close() })
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + monitor.addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("health=%d", response.StatusCode)
	}
	stop, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := monitor.Stop(stop); err != nil {
		t.Fatal(err)
	}
	if monitor.ready.Load() {
		t.Fatal("shutdown left process ready")
	}
	select {
	case err := <-monitor.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-stop.Done():
		t.Fatal("listener did not terminate")
	}
	if response, err := client.Get("http://" + monitor.addr + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Fatal("listener accepted request after shutdown")
	}
}

type fixedMetricSource struct{}

func (fixedMetricSource) Snapshot(context.Context) (observability.WorkerSnapshot, error) {
	return observability.WorkerSnapshot{Pending: 1, DeadLetters: 1}, nil
}

func TestWorkerListenerFailureStopsStartedJobs(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	jobs := &testJobs{}
	cleaned := false
	factory := func(context.Context, func(string) string, outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
		return &observedWorker{jobWorker: jobs, source: fixedMetricSource{}}, nil, func() { cleaned = true }, nil
	}
	err = runContext(context.Background(), env(map[string]string{"OSC_WORKER_ADDR": occupied.Addr().String()}), nil, factory)
	if err == nil || err.Error() != "worker monitor listener unavailable" || !jobs.started || !jobs.stopped || !cleaned {
		t.Fatalf("listener failure lifecycle error=%v started=%v stopped=%v cleanup=%v", err, jobs.started, jobs.stopped, cleaned)
	}
}
