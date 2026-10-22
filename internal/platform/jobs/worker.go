//go:build pgx

// Worker admits only a registered, signed kind before calling the controlled
// inventory service inside tenanttx. Periodic expiry runs once per interval:
// SKIP LOCKED returning zero does not prove that every due row was processed.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const rollbackTimeout = 5 * time.Second

// Leave the command's outer 30-second Stop deadline time to persist River's
// cancellation/retry state. A zero River timeout otherwise waits indefinitely.
const maxSoftStopTimeout = 25 * time.Second

type Expirer interface {
	Expire(context.Context, string, int) error
}
type ControlledInventory struct{ Pool pgxadapter.QueueDB }

func (s ControlledInventory) Expire(ctx context.Context, tenant string, batch int) error {
	return pgxadapter.InQueueTenant(ctx, s.Pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := db.New(tx).ExpireInventoryJob(ctx, int32(batch))
		return err
	})
}

type ExportSweeper interface {
	Sweep(context.Context, string) error
}

type InventoryWorker struct {
	river.WorkerDefaults[InventoryExpireArgs]
	Signer  *Signer
	Service Expirer
	Sweeper ExportSweeper
}

func (w *InventoryWorker) Work(ctx context.Context, job *river.Job[InventoryExpireArgs]) error {
	if job == nil || job.JobRow == nil || job.Kind != InventoryExpireKind {
		return river.JobCancel(errors.New("unknown job kind"))
	}
	if err := w.Signer.Validate(job.Args); err != nil {
		return river.JobCancel(err)
	}
	if w.Service == nil {
		return errors.New("inventory service unavailable")
	}
	err := w.Service.Expire(ctx, job.Args.TenantID, job.Args.Batch)
	if w.Sweeper != nil {
		// Reclamation must still run when inventory expiry fails; both paths
		// are idempotent, so a combined failure safely retries the tenant job.
		return errors.Join(err, w.Sweeper.Sweep(ctx, job.Args.TenantID))
	}
	return err
}

// Tenants come from trusted deployment configuration, never from pending jobs.
// Empty Tenants disables periodic producers while still processing queued jobs.
type WorkerConfig struct {
	ExportSweeper ExportSweeper
	ExportService ExportBuilder
	Tenants       []string
	Batch         int
	Interval      time.Duration
}

// An unregistered kind is a permanent admission failure, not a transient
// service error. Keep normal failures/panics on River's recovery retry path.
type admissionErrors struct{}

func (admissionErrors) HandleError(_ context.Context, _ *rivertype.JobRow, err error) *river.ErrorHandlerResult {
	var unknown *rivertype.UnknownJobKindError
	if errors.As(err, &unknown) {
		return &river.ErrorHandlerResult{SetCancelled: true}
	}
	return nil
}
func (admissionErrors) HandlePanic(context.Context, *rivertype.JobRow, any, string) *river.ErrorHandlerResult {
	return nil
}

func NewWorker(pool *pgxadapter.QueuePool, signer *Signer, cfg WorkerConfig) (*river.Client[pgx.Tx], error) {
	if pool == nil {
		return nil, errors.New("worker pool required")
	}
	return NewWorkerWithService(pool.RiverDriver(), signer, ControlledInventory{Pool: pool}, cfg, river.Config{})
}

// NewWorkerWithService is a trusted infrastructure composition port for an
// Expirer implementation and River scheduling settings. Admission, schema,
// queues, workers and periodic tenant scope remain fixed by this package;
// tuning must never come from request or job payload data.
// Positive shutdown grace may be shortened, but cannot disable cancellation or
// exceed the command's outer shutdown deadline.
func NewWorkerWithService(driver *riverpgxv5.Driver, signer *Signer, service Expirer, cfg WorkerConfig, tuning river.Config) (*river.Client[pgx.Tx], error) {
	return buildWorker(driver, signer, service, cfg, tuning)
}

func buildWorker(driver *riverpgxv5.Driver, signer *Signer, service Expirer, cfg WorkerConfig, tuning river.Config) (*river.Client[pgx.Tx], error) {
	if driver == nil || signer == nil || len(signer.key) < 32 || service == nil {
		return nil, errors.New("invalid worker configuration")
	}
	if cfg.Batch == 0 {
		cfg.Batch = 100
	}
	if cfg.Batch < 1 || cfg.Batch > 1000 {
		return nil, errors.New("invalid expiry batch")
	}
	if cfg.Interval == 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Interval < time.Second {
		return nil, errors.New("expiry interval must be at least one second")
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &InventoryWorker{Signer: signer, Service: service, Sweeper: cfg.ExportSweeper})
	if cfg.ExportService != nil {
		river.AddWorker(workers, &CustomerExportWorker{Signer: signer, Service: cfg.ExportService})
	}
	tuning.Schema, tuning.Workers = "queue", workers
	tuning.ErrorHandler = admissionErrors{}
	tuning.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}}
	if cfg.ExportService != nil {
		tuning.Queues[CustomerDataExportQueue] = river.QueueConfig{MaxWorkers: 1}
	}
	tuning.ReindexerIndexNames = []string{} // worker has no DDL/REINDEX rights.
	if tuning.SoftStopTimeout <= 0 || tuning.SoftStopTimeout > maxSoftStopTimeout {
		tuning.SoftStopTimeout = maxSoftStopTimeout
	}
	tuning.PeriodicJobs = nil
	seen := map[string]bool{}
	for _, tenant := range cfg.Tenants {
		args, err := signer.inventoryArgs(tenant, cfg.Batch)
		if err != nil {
			return nil, err
		}
		if seen[args.TenantID] {
			return nil, errors.New("duplicate periodic tenant")
		}
		seen[args.TenantID] = true
		tuning.PeriodicJobs = append(tuning.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.Interval), func() (river.JobArgs, *river.InsertOpts) { return args, nil }, &river.PeriodicJobOpts{RunOnStart: true}))
	}
	return river.NewClient(driver, &tuning)
}
