// Responsibility: worker health and bounded aggregate scrape contract tests.
// Dependencies: observability and stdlib; called by local and race Go gates.
// Invariants: readiness gates health; failed collection never exports false zeros.
package observability

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type workerSource func(context.Context) (WorkerSnapshot, error)

func (s workerSource) Snapshot(ctx context.Context) (WorkerSnapshot, error) { return s(ctx) }

func TestWorkerHealthAndAggregateMetrics(t *testing.T) {
	var ready atomic.Bool
	source := workerSource(func(ctx context.Context) (WorkerSnapshot, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Error("scrape must have a bounded deadline")
		}
		return WorkerSnapshot{Pending: 3, DeadLetters: 2, OldestPendingAge: 1500 * time.Millisecond}, nil
	})
	h := NewWorkerHandler(ready.Load, source)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatalf("before readiness=%d", w.Code)
	}
	ready.Store(true)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("health=%d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics=%d", w.Code)
	}
	for _, want := range []string{
		"opensite_outbox_pending 3", "opensite_outbox_dead_letters 2", "opensite_outbox_oldest_pending_age_seconds 1.5",
		`opensite_http_requests_total{method="GET",status="204"} 1`,
		`opensite_http_errors_total{method="GET",status="503"} 1`,
		`opensite_http_request_duration_seconds_count{method="GET"} 2`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %q in %q", want, w.Body.String())
		}
	}
	ready.Store(false)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatal("stopping process must not be healthy")
	}
}

func TestWorkerMetricsFailuresAndMethods(t *testing.T) {
	calls := 0
	h := NewWorkerHandler(func() bool { return true }, workerSource(func(context.Context) (WorkerSnapshot, error) {
		calls++
		return WorkerSnapshot{}, errors.New("secret tenant payload")
	}))
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/metrics", 405}, {"HEAD", "/metrics", 200}, {"POST", "/healthz", 405}, {"GET", "/missing", 404},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s=%d", tc.method, tc.path, w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("HEAD and rejected methods must not collect")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "opensite_outbox_pending 0") {
		t.Fatalf("collection failure leaked details or zero metrics: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	NewWorkerHandler(nil, nil).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatal("nil readiness must fail closed")
	}
}
