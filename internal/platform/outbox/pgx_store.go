//go:build pgx

// Responsibility: isolated committed tenant transactions for outbox Store calls.
// Dependencies: generated db queries, pgx and existing tenanttx pgxadapter.
// Called by Dispatcher; pool must authenticate as app_outbox_worker, never an
// owner/superuser. Tenant comes from trusted worker composition, not event input.
package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) Claim(ctx context.Context, tenantID, workerID string, limit, seconds int32) ([]Lease, error) {
	var leases []Lease
	err := (pgxadapter.Runner{Pool: s.Pool}).InTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := db.New(tx).ClaimOutboxLease(ctx, db.ClaimOutboxLeaseParams{TenantID: tenantID, WorkerID: workerID, ClaimLimit: limit, LeaseSeconds: seconds})
		if err != nil {
			return err
		}
		for _, row := range rows {
			leases = append(leases, Lease{Delivery: Delivery{TenantID: row.TenantID, EventID: row.EventID, Target: row.DeliveryTarget, DedupKey: row.DedupKey, EventType: row.EventType, Payload: []byte(row.Payload)}, WorkerID: row.WorkerID, Attempt: row.Attempt, LeaseUntil: time.UnixMilli(row.LeaseUntilMillis)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	} // Commit failure never exposes an apparently owned lease.
	return leases, nil
}

func (s PGStore) Complete(ctx context.Context, l Lease) (bool, error) {
	var accepted bool
	err := (pgxadapter.Runner{Pool: s.Pool}).InTenant(ctx, l.Delivery.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = db.New(tx).CompleteOutboxLease(ctx, db.CompleteOutboxLeaseParams{TenantID: l.Delivery.TenantID, EventID: l.Delivery.EventID, WorkerID: l.WorkerID, Attempt: l.Attempt})
		return err
	})
	return accepted && err == nil, err
}

func (s PGStore) Fail(ctx context.Context, l Lease, code string, retry int32) (bool, error) {
	if code != "OUTBOX_SEND_FAILED" {
		return false, errors.New("OUTBOX_UNSAFE_FAILURE_CODE")
	}
	var accepted bool
	err := (pgxadapter.Runner{Pool: s.Pool}).InTenant(ctx, l.Delivery.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = db.New(tx).FailOutboxLease(ctx, db.FailOutboxLeaseParams{TenantID: l.Delivery.TenantID, EventID: l.Delivery.EventID, WorkerID: l.WorkerID, Attempt: l.Attempt, ErrorCode: code, RetrySeconds: retry})
		return err
	})
	return accepted && err == nil, err
}
