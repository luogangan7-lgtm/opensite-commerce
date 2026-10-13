// Responsibility: worker health and aggregate outbox Prometheus telemetry.
// Dependencies: controlled WorkerMetrics and stdlib; called by cmd/worker.
// Invariants: no tenant/payload labels, bounded collection, no false zero on failure.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// WorkerSnapshot contains aggregate counts only, never tenant or payload data.
type WorkerSnapshot struct {
	Pending          uint64
	DeadLetters      uint64
	OldestPendingAge time.Duration
}

// WorkerMetrics reads the restricted aggregate function through the outbox role.
type WorkerMetrics interface {
	Snapshot(context.Context) (WorkerSnapshot, error)
}

// NewWorkerHandler serves process readiness and bounded database telemetry.
// The caller marks ready only after jobs startup, and clears it before shutdown.
func NewWorkerHandler(ready func() bool, source WorkerMetrics) http.Handler {
	var requests Metrics
	health := requests.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if ready == nil || !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			health.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		if source == nil {
			http.Error(w, "worker metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		snapshot, err := source.Snapshot(ctx)
		if err != nil {
			http.Error(w, "worker metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		requests.write(w)
		fmt.Fprintf(w, "# HELP opensite_outbox_pending Undelivered non-dead-letter events across application tenants.\n# TYPE opensite_outbox_pending gauge\nopensite_outbox_pending %d\n", snapshot.Pending)
		fmt.Fprintf(w, "# HELP opensite_outbox_dead_letters Events requiring explicit operator recovery.\n# TYPE opensite_outbox_dead_letters gauge\nopensite_outbox_dead_letters %d\n", snapshot.DeadLetters)
		fmt.Fprintf(w, "# HELP opensite_outbox_oldest_pending_age_seconds Age of the oldest undelivered non-dead-letter event.\n# TYPE opensite_outbox_oldest_pending_age_seconds gauge\nopensite_outbox_oldest_pending_age_seconds %g\n", snapshot.OldestPendingAge.Seconds())
	})
}
