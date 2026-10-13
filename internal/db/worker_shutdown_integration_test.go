//go:build pgx

// Worker shutdown gate shares the serial package with all River integration gates.
// Dependencies: opaque pgxadapter, jobs and disposable PG; callers: go test.
// Invariant: grace expiration cancels a running job before the outer deadline.
package db_test

import (
	"context"
	"crypto/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river"
)

type shutdownExpirer struct {
	started, cancelled chan struct{}
	once               sync.Once
}

func (s *shutdownExpirer) Expire(ctx context.Context, _ string, _ int) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	close(s.cancelled)
	return ctx.Err()
}

func TestWorkerStopCancelsAfterDefaultGrace(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if os.Getenv("JOBS_REQUIRE_PG") == "1" {
			t.Fatal("DATABASE_URL required")
		}
		t.Skip("NOT_RUN: disposable PostgreSQL absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	admin, err := pgxadapter.OpenMigrationPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	pool, err := pgxadapter.OpenQueuePool(ctx, dsn, "app_queue_worker")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var tenant string
	if err := admin.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('H2 worker shutdown fixture') RETURNING id::text").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer, err := jobs.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	service := &shutdownExpirer{started: make(chan struct{}), cancelled: make(chan struct{})}
	client, err := jobs.NewWorkerWithService(pool.RiverDriver(), signer, service, jobs.WorkerConfig{}, river.Config{TestOnly: true, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond, JobTimeout: time.Minute, RescueStuckJobsAfter: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.StopAndCancel(cleanup); err != nil {
			t.Errorf("worker cleanup: %v", err)
		}
		if _, err := admin.Exec(cleanup, "DELETE FROM queue.river_job WHERE args->>'tenant_id'=$1", tenant); err != nil {
			t.Errorf("job cleanup: %v", err)
		}
	}()
	// Queue tests share one serial package: workers must not consume each other's jobs.
	runner, closeRunner, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRunner()
	enqueue, err := jobs.NewEnqueuer(signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := enqueue.InventoryExpireTx(ctx, tx, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.started:
	case <-ctx.Done():
		t.Fatal("worker never entered job")
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 28*time.Second)
	defer stop()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("grace must cancel running task before outer shutdown deadline: %v", err)
	}
	select {
	case <-service.cancelled:
	default:
		t.Fatal("Stop returned without cancelling the running job")
	}
}
