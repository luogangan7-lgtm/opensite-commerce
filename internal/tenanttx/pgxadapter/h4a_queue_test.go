// Responsibilities: prove deadlines on the real one-connection pool queue.
// Dependencies: expendable CI PostgreSQL and private pool access for occupancy.
// Invariant: caller and internal budgets include acquisition, before callbacks.
package pgxadapter

import (
	"context"
	"os"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

func TestH4ARealPoolQueueBudgets(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL required")
	}
	r, err := OpenStaffAuthentication(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	held, err := r.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	for _, budget := range []time.Duration{20 * time.Millisecond, 3 * time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		before := r.pool.Stat().CanceledAcquireCount()
		started := time.Now()
		err := r.RunAuthentication(ctx, func(context.Context, *db.Queries) error {
			t.Error("occupied pool admitted callback")
			return nil
		})
		cancel()
		if err == nil || r.pool.Stat().CanceledAcquireCount() != before+1 || time.Since(started) > 2500*time.Millisecond {
			t.Fatal("actual pooled queue ignored caller/internal deadline")
		}
	}
}

func TestH4ASessionPoolQueueMetrics(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL required")
	}
	r, err := OpenStaffAuthentication(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	held, err := r.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var metrics observability.Metrics
	handler := metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 20*time.Millisecond)
		defer cancel()
		if err := r.RunAuthentication(ctx, func(context.Context, *db.Queries) error {
			t.Error("occupied session pool reached callback")
			return nil
		}); err == nil {
			t.Error("occupied session pool ignored deadline")
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/private", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`opensite_auth_operations_total{stage="staff_pool",outcome="timeout"} 1`,
		`opensite_auth_duration_seconds_count{stage="staff_queue"} 1`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing queue metric %q in %s", want, w.Body.String())
		}
	}
}
