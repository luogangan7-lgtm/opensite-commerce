//go:build pgx

// Responsibility: queue-only worker configuration compatibility regressions.
// Dependencies: real disposable PG through approved pool factories; Go pgx gates.
// Invariants: absent optional outbox is allowed; configured failures never degrade.
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

// Real composition regression: deployments without the optional outbox
// capability still start River and expose healthy process monitoring.
func TestLegacyWorkerWithoutOutboxPG(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: DATABASE_URL absent for worker composition")
	}
	for _, mode := range []string{"", "false"} {
		t.Run("outbox_mode_"+mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			settings := map[string]string{"WORKER_DATABASE_URL": dsn, "JOB_SIGNING_KEY": strings.Repeat("11", 32), "OUTBOX_ENABLED": mode}
			worker, runner, cleanup, err := composeWorkers(ctx, env(settings), nil)
			if err != nil {
				t.Fatalf("legacy queue-only composition failed: %v", err)
			}
			defer cleanup()
			if runner != nil {
				t.Fatal("disabled outbox unexpectedly has a dispatcher")
			}
			observed, ok := worker.(interface {
				MetricsSource() observability.WorkerMetrics
			})
			if !ok || observed.MetricsSource() != nil {
				t.Fatal("missing capability should explicitly have no metric source")
			}
			// Start consumes the default queue. Root's package-parallel DB suite
			// must not let this client consume other tests' signed job fixtures.
			// The child isolated PG acceptance explicitly opts in to Start/Stop.
			if os.Getenv("WORKER_START_EXCLUSIVE_PG") == "1" {
				if err := worker.Start(ctx); err != nil {
					t.Fatalf("River did not start: %v", err)
				}
				if err := worker.Stop(ctx); err != nil {
					t.Fatalf("River did not stop: %v", err)
				}
			}
		})
	}
}

func TestConfiguredOutboxMetricsPG(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: DATABASE_URL absent for worker composition")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	settings := map[string]string{"WORKER_DATABASE_URL": dsn, "OUTBOX_DATABASE_URL": dsn, "JOB_SIGNING_KEY": strings.Repeat("11", 32), "OUTBOX_ENABLED": "false"}
	worker, runner, cleanup, err := composeWorkers(ctx, env(settings), nil)
	if err != nil {
		t.Fatalf("configured composition failed: %v", err)
	}
	defer cleanup()
	if runner != nil {
		t.Fatal("metrics credentials enabled dispatch")
	}
	observed, ok := worker.(interface {
		MetricsSource() observability.WorkerMetrics
	})
	if !ok || observed.MetricsSource() == nil {
		t.Fatal("configured metrics were silently disabled")
	}
	if _, err := observed.MetricsSource().Snapshot(ctx); err != nil {
		t.Fatalf("real controlled metrics unavailable: %v", err)
	}
	for _, partial := range []map[string]string{
		{"OPENSITE_OUTBOX_DB_PASSWORD": "short"},
		{"OPENSITE_OUTBOX_DB_PASSWORD": strings.Repeat("x", 32)},
		{"OUTBOX_DATABASE_URL": "invalid DSN"},
	} {
		invalid := map[string]string{"WORKER_DATABASE_URL": dsn, "JOB_SIGNING_KEY": strings.Repeat("11", 32), "OUTBOX_ENABLED": "false"}
		for key, value := range partial {
			invalid[key] = value
		}
		if _, _, cleanup, err := composeWorkers(ctx, env(invalid), nil); err == nil {
			cleanup()
			t.Fatal("partially configured outbox capability was silently disabled")
		}
	}
}
