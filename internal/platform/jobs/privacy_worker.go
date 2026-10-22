//go:build pgx

// Responsibilities: validate signed native River exports before controlled
// data-rights service access. Dependencies: River + approved service port;
// WorkerConfig composition calls it. Queue role never gains customer PII access.
package jobs

import (
	"context"
	"errors"
	"github.com/riverqueue/river"
)

type ExportBuilder interface {
	BuildExport(context.Context, string, string) error
}
type CustomerExportWorker struct {
	river.WorkerDefaults[CustomerDataExportArgs]
	Signer  *Signer
	Service ExportBuilder
}

func (w *CustomerExportWorker) Work(ctx context.Context, job *river.Job[CustomerDataExportArgs]) error {
	if job == nil || job.JobRow == nil || job.Kind != CustomerDataExportKind {
		return river.JobCancel(errors.New("unknown export job kind"))
	}
	if err := w.Signer.ValidateCustomerExport(job.Args); err != nil {
		return river.JobCancel(err)
	}
	if w.Service == nil {
		return river.JobCancel(errors.New("export service unavailable"))
	}
	return w.Service.BuildExport(ctx, job.Args.TenantID, job.Args.RequestID)
}
