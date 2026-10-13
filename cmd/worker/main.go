//go:build pgx

// Responsibility: jobs and outbox worker startup, capabilities and graceful stop.
// Dependencies: jobs, outbox, pgx pools, environment/signals; called by main.
// Invariants: queue/outbox roles use separate pools; dispatch defaults off;
// only explicit local SMTP uses fenced, notification-only delivery claims.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river"
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
	sender, err := configuredSender(os.Getenv)
	if err != nil {
		return err
	}
	return runContext(ctx, os.Getenv, sender, composeWorkers)
}

// Only explicit local SMTP configuration registers the dedicated adapter.
func configuredSender(getenv func(string) string) (outbox.Sender, error) {
	sender, err := notifications.SenderFromEnv(getenv)
	if err != nil || sender == nil {
		return nil, err
	}
	return configuredNotificationSender{sender: sender}, nil
}

type configuredNotificationSender struct{ sender notifications.Sender }

func (configuredNotificationSender) Send(context.Context, outbox.Delivery) error {
	return outbox.ErrInvalidInput
}

func dispatchComponents(pool pgxadapter.QueueDB, sender outbox.Sender) (outbox.Store, outbox.Sender) {
	if configured, ok := sender.(configuredNotificationSender); ok {
		return notifications.OutboxStore(pool), notifications.NewWorker(pool, configured.sender)
	}
	return outbox.PGStore{Pool: pool}, sender
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
	if enabled == "true" {
		url, err := workerDSN(getenv, "OUTBOX_DATABASE_URL", "OUTBOX", "opensite_outbox_login")
		if err != nil {
			return err
		}
		if url == "" {
			return errors.New("OUTBOX_DATABASE_URL required")
		}
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
	var monitor *workerMonitor
	var monitorDone <-chan error
	if observed, ok := worker.(interface {
		MetricsSource() observability.WorkerMetrics
	}); ok {
		monitor, err = startWorkerMonitor(getenv("OSC_WORKER_ADDR"), observed.MetricsSource())
		if err != nil {
			stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			_ = worker.Stop(stop)
			return err
		}
		monitorDone = monitor.done
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
	case <-monitorDone:
		runErr = errors.New("worker monitor stopped unexpectedly")
	}
	cancel()
	stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	var monitorErr, stopErr error
	if monitor != nil {
		monitorErr, stopErr = stopConcurrently(stop, monitor, worker)
	} else {
		stopErr = worker.Stop(stop)
	}
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
	if monitorErr != nil {
		return monitorErr
	}
	return nil
}

func composeWorkers(ctx context.Context, getenv func(string) string, sender outbox.Sender) (jobWorker, *outbox.Runner, func(), error) {
	if getenv("PRIVACY_WORKER_DATABASE_URL") != "" {
		root := getenv("OPENSITE_MEDIA_ROOT")
		legacy := getenv("OPENSITE_PRIVACY_ROOT")
		if !filepath.IsAbs(root) || (legacy != "" && filepath.Clean(legacy) != filepath.Clean(root)) {
			return nil, nil, nil, errors.New("privacy root must equal the configured media root")
		}
	}
	url, err := workerDSN(getenv, "WORKER_DATABASE_URL", "WORKER", "opensite_queue_login")
	if err != nil {
		return nil, nil, nil, err
	}
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
	cfg := jobs.WorkerConfig{Tenants: tenants}
	if privacyURL := getenv("PRIVACY_WORKER_DATABASE_URL"); privacyURL != "" {
		// Separate approved runtime login: queue credentials cannot read PII. Only
		// the signed export worker calls this runner with validated internal IDs.
		privacyRunner, closePrivacy, openErr := pgxadapter.Open(startupCtx, privacyURL)
		if openErr != nil {
			return nil, nil, nil, errors.New("privacy worker database unavailable")
		}
		cleanup = func() { closePrivacy(); pool.Close() }
		tokenKey, keyErr := hex.DecodeString(getenv("PRIVACY_TOKEN_KEY"))
		if keyErr != nil {
			return nil, nil, nil, errors.New("invalid privacy token key")
		}
		exports, exportErr := datarights.New(rights.Config{Run: privacyRunner.InTenant, Root: getenv("OPENSITE_MEDIA_ROOT"), TokenKey: tokenKey})
		if exportErr != nil {
			return nil, nil, nil, errors.New("privacy worker configuration unavailable")
		}
		cfg.ExportService = permanentExports{exports}
	}
	worker, err := jobs.NewWorker(pool, signer, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	outboxURL, err := workerDSN(getenv, "OUTBOX_DATABASE_URL", "OUTBOX", "opensite_outbox_login")
	if err != nil {
		return nil, nil, nil, err
	}
	if outboxURL == "" {
		if getenv("OUTBOX_ENABLED") == "true" {
			return nil, nil, nil, errors.New("OUTBOX_DATABASE_URL required")
		}
		// Queue-only deployments keep health monitoring; metrics explicitly
		// report unavailable instead of requiring an optional capability.
		success = true
		return &observedWorker{jobWorker: worker}, nil, cleanup, nil
	}
	outboxPool, err := pgxadapter.OpenQueuePool(startupCtx, outboxURL, "app_outbox_worker")
	if err != nil {
		return nil, nil, nil, errors.New("outbox database capability unavailable")
	}
	priorCleanup := cleanup
	cleanup = func() { outboxPool.Close(); priorCleanup() }
	var runner *outbox.Runner
	if getenv("OUTBOX_ENABLED") == "true" {
		store, destination := dispatchComponents(outboxPool, sender)
		runner = &outbox.Runner{Source: outbox.PGTenantSource{Pool: outboxPool}, Dispatcher: outbox.Dispatcher{Store: store, Sender: destination, Config: outbox.Config{WorkerID: uuid.NewString(), LeaseSeconds: 60, BaseRetrySeconds: 2, MaxRetrySeconds: 300}}, PollInterval: time.Second, CycleTimeout: 30 * time.Second, TenantBatchSize: 100, EventsPerTenant: 10}
	}
	success = true
	return &observedWorker{jobWorker: worker, source: outboxMetrics{query: outboxPool}}, runner, cleanup, nil
}

// permanentExports cancels export jobs no retry can change: invalid or over
// limit, missing, or expired/consumed/revoked/erased requests. Busy and
// unavailable failures keep River's retry path.
type permanentExports struct{ jobs.ExportBuilder }

func (p permanentExports) BuildExport(ctx context.Context, tenant, request string) error {
	err := p.ExportBuilder.BuildExport(ctx, tenant, request)
	if errors.Is(err, rights.ErrInvalid) || errors.Is(err, rights.ErrConflict) || errors.Is(err, rights.ErrNotFound) {
		return river.JobCancel(err)
	}
	return err
}

// stopConcurrently starts both stops at once so a slow monitor drain cannot
// consume River's soft-stop grace before it begins (shared 30s budget).
func stopConcurrently(ctx context.Context, monitor, worker interface{ Stop(context.Context) error }) (monitorErr, workerErr error) {
	done := make(chan error, 1)
	go func() { done <- monitor.Stop(ctx) }()
	workerErr = worker.Stop(ctx)
	return <-done, workerErr
}
