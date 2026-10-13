//go:build pgx

// Responsibility: jobs and outbox worker startup, capabilities and graceful stop.
// Dependencies: jobs, outbox, pgx pools, environment/signals; called by main.
// Invariants: queue/outbox roles use separate pools; OUTBOX_ENABLED defaults off
// and no production Sender is registered. No unconfigured event is ever claimed.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Only tests inject a Sender. Enabling dispatch in this executable fails closed
	// until a separately reviewed destination-validation/dedup adapter is added.
	return runContext(ctx, os.Getenv, nil, composeWorkers)
}

// jobWorker keeps startup orchestration injectable without replacing Dispatcher.
// Tests use the same runContext path as main with controlled jobs and a test Sender.
type jobWorker interface {
	Start(context.Context) error
	Stop(context.Context) error
}
type workerFactory func(context.Context, func(string) string, outbox.Sender) (jobWorker, *outbox.Runner, func(), error)

func runContext(parent context.Context, getenv func(string) string, sender outbox.Sender, compose workerFactory) error {
	enabled := getenv("OUTBOX_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return errors.New("invalid OUTBOX_ENABLED")
	}
	if enabled == "true" && sender == nil {
		return errors.New("OUTBOX_SENDER_NOT_CONFIGURED")
	}
	if enabled == "true" && getenv("OUTBOX_DATABASE_URL") == "" {
		return errors.New("OUTBOX_DATABASE_URL required")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	worker, runner, cleanup, err := compose(ctx, getenv, sender)
	if err != nil {
		return err
	}
	defer cleanup()
	// River turns a canceled Start context into StopAndCancel. Detach it so a
	// signal reaches running jobs only through Stop's 30s grace window below.
	if err := worker.Start(context.WithoutCancel(ctx)); err != nil {
		return errors.New("worker startup failed")
	}
	runnerDone := make(chan error, 1)
	if runner != nil {
		go func() { runnerDone <- runner.Run(ctx) }()
	}
	var runErr error
	joined := false
	select {
	case <-ctx.Done():
	case runErr = <-runnerDone:
		joined = true
	}
	cancel()
	stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	stopErr := worker.Stop(stop)
	if runner != nil && !joined {
		select {
		case runErr = <-runnerDone:
		case <-stop.Done():
			return errors.New("outbox worker stop timed out")
		}
	}
	if runErr != nil {
		return runErr
	}
	if stopErr != nil {
		return errors.New("worker stop failed")
	}
	return nil
}

func composeWorkers(ctx context.Context, getenv func(string) string, sender outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
	url := getenv("WORKER_DATABASE_URL")
	if url == "" {
		return nil, nil, nil, errors.New("WORKER_DATABASE_URL required")
	}
	key, err := hex.DecodeString(getenv("JOB_SIGNING_KEY"))
	if err != nil {
		return nil, nil, nil, errors.New("JOB_SIGNING_KEY must be hexadecimal")
	}
	signer, err := jobs.NewSigner(key)
	if err != nil {
		return nil, nil, nil, err
	}
	startupCtx, startupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startupCancel()
	pool, err := pgxadapter.OpenQueuePool(startupCtx, url, "app_queue_worker")
	if err != nil {
		return nil, nil, nil, errors.New("worker database capability unavailable")
	}
	cleanup := pool.Close
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	var tenants []string
	if value := strings.TrimSpace(getenv("JOB_TENANTS")); value != "" {
		for _, tenant := range strings.Split(value, ",") {
			tenants = append(tenants, strings.TrimSpace(tenant))
		}
	}
	worker, err := jobs.NewWorker(pool, signer, jobs.WorkerConfig{Tenants: tenants})
	if err != nil {
		return nil, nil, nil, err
	}
	var runner *outbox.Runner
	if getenv("OUTBOX_ENABLED") == "true" {
		outboxPool, err := pgxadapter.OpenQueuePool(startupCtx, getenv("OUTBOX_DATABASE_URL"), "app_outbox_worker")
		if err != nil {
			return nil, nil, nil, err
		}
		cleanup = func() { outboxPool.Close(); pool.Close() }
		runner = &outbox.Runner{Source: outbox.PGTenantSource{Pool: outboxPool}, Dispatcher: outbox.Dispatcher{Store: outbox.PGStore{Pool: outboxPool}, Sender: sender, Config: outbox.Config{WorkerID: uuid.NewString(), LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 300}}, PollInterval: time.Second, CycleTimeout: 30 * time.Second, TenantBatchSize: 100, EventsPerTenant: 10}
	}
	success = true
	return worker, runner, cleanup, nil
}
