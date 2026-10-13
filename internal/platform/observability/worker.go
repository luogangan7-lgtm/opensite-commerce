// Worker telemetry defines a future reliability-module seam, with stdlib types.
// A future approved adapter may implement WorkerMetrics; bootstrap does not call it.
// NOT_RUN: main has no merged worker, so no worker/outbox database poller is present.
package observability

import (
	"context"
	"time"
)

// WorkerSnapshot contains aggregate counts only, never tenant or payload data.
type WorkerSnapshot struct {
	Pending          uint64
	OldestPendingAge time.Duration
	Attempts         uint64
	Failures         uint64
}

// WorkerMetrics is reserved for the merged reliability implementation's adapter.
// No zero-valued gauges are exported to falsely imply a functioning worker.
type WorkerMetrics interface {
	Snapshot(context.Context) (WorkerSnapshot, error)
}
