//go:build pgx

// Worker shutdown gates exercise admission construction and River cancellation.
// Dependencies: the opaque pgxadapter and disposable PG; callers: go test.
// Invariants: nil capabilities return errors; shutdown cancels running jobs.
package jobs

import (
	"context"
	"crypto/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river"
)

func TestNewWorkerRejectsNilPool(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Signer{nil, signer} {
		if client, err := NewWorker(nil, s, WorkerConfig{}); err == nil || client != nil {
			t.Fatal("nil pool must fail before driver construction")
		}
	}
}

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
	signer, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	service := &shutdownExpirer{started: make(chan struct{}), cancelled: make(chan struct{})}
	client, err := NewWorkerWithService(pool.RiverDriver(), signer, service, WorkerConfig{}, river.Config{TestOnly: true, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond, JobTimeout: time.Minute, RescueStuckJobsAfter: 2 * time.Minute})
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
	producer, err := river.NewClient(admin.RiverDriver(), &river.Config{Schema: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	args, err := signer.inventoryArgs(tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Insert(ctx, args, nil); err != nil {
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
