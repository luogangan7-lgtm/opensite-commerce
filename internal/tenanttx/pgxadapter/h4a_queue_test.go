// Responsibilities: prove deadlines on the real one-connection pool queue.
// Dependencies: expendable CI PostgreSQL and private pool access for occupancy.
// Invariant: caller and internal budgets include acquisition, before callbacks.
package pgxadapter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
)

func TestH4ARealPoolQueueBudgets(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" { pgxtest.Skip(t, "NOT_RUN: disposable PostgreSQL required") }
	r, err := OpenStaffAuthentication(context.Background(), dsn)
	if err != nil { t.Fatal(err) }
	defer r.Close(context.Background())
	held, err := r.pool.Acquire(context.Background())
	if err != nil { t.Fatal(err) }
	defer held.Release()
	for _, budget := range []time.Duration{20*time.Millisecond, 3*time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		before := r.pool.Stat().CanceledAcquireCount()
		started := time.Now()
		err := r.RunAuthentication(ctx, func(context.Context, *db.Queries) error { t.Error("occupied pool admitted callback"); return nil })
		cancel()
		if err == nil || r.pool.Stat().CanceledAcquireCount() != before+1 || time.Since(started) > 2500*time.Millisecond {
			t.Fatal("actual pooled queue ignored caller/internal deadline")
		}
	}
}
