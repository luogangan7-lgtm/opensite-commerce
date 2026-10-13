// Responsibility: notification-only outbox claims and committed send commands.
// Dependencies: sqlc, tenanttx QueueDB; composition passes app_outbox_worker only.
// Invariants: no table writes, lease identity copied without weakening fences,
// each command commits before returning; network IO never holds a transaction.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"strconv"
)

type Worker struct{ Pool pgxadapter.QueueDB }

func (w Worker) leaseTx(ctx context.Context, l outbox.Lease, fn func(context.Context, *db.Queries) error) error {
	return pgxadapter.InQueueTenant(ctx, w.Pool, l.Delivery.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT set_config('app.outbox_worker',$1,true),set_config('app.outbox_attempt',$2,true)`, l.WorkerID, strconv.Itoa(int(l.Attempt))); e != nil {
			return e
		}
		return fn(ctx, db.New(tx))
	})
}
func (w Worker) Begin(ctx context.Context, l outbox.Lease, id string) (service.Envelope, error) {
	var row service.Envelope
	e := w.leaseTx(ctx, l, func(ctx context.Context, q *db.Queries) error {
		r, e := q.BeginNotification(ctx, db.BeginNotificationParams{TenantID: UUID(l.Delivery.TenantID), DispatchEventID: UUID(l.Delivery.EventID), NotificationID: UUID(id)})
		if e != nil {
			return e
		}
		row = service.Envelope{ID: ID(r.ID), EventID: ID(r.SourceEventID), Recipient: r.Recipient, Locale: r.Locale, TemplateKey: r.TemplateKey, Reference: ID(r.ReferenceID), Status: r.Status, Token: ID(r.SendToken)}
		return nil
	})
	if e != nil {
		return service.Envelope{}, MapError(e)
	}
	return row, nil
}
func (w Worker) Finish(ctx context.Context, l outbox.Lease, row service.Envelope, outcome, code string) (bool, error) {
	var accepted bool
	e := w.leaseTx(ctx, l, func(ctx context.Context, q *db.Queries) error {
		var e error
		accepted, e = q.FinishNotification(ctx, db.FinishNotificationParams{TenantID: UUID(l.Delivery.TenantID), NotificationID: UUID(row.ID), SendToken: UUID(row.Token), Outcome: outcome, SafeError: pgtype.Text{String: code, Valid: code != ""}})
		return e
	})
	return accepted && e == nil, MapError(e)
}
func (w Worker) Claim(ctx context.Context, tenant, worker string, limit, seconds int32) ([]outbox.Lease, error) {
	rows := make([]outbox.Lease, 0)
	e := pgxadapter.InQueueTenant(ctx, w.Pool, tenant, func(ctx context.Context, tx pgx.Tx) error {
		result, e := db.New(tx).ClaimNotificationOutbox(ctx, db.ClaimNotificationOutboxParams{TenantID: UUID(tenant), WorkerID: UUID(worker), ClaimLimit: limit, LeaseSeconds: seconds})
		if e != nil {
			return e
		}
		for _, r := range result {
			rows = append(rows, outbox.Lease{Delivery: outbox.Delivery{TenantID: ID(r.TenantID), EventID: ID(r.ID), Target: r.DeliveryTarget, DedupKey: r.DedupKey, EventType: r.EventType, Payload: append([]byte(nil), r.Payload...)}, WorkerID: ID(r.LeaseOwner), Attempt: r.Attempts, LeaseUntil: r.LeaseUntil.Time})
		}
		return nil
	})
	if e != nil {
		return nil, MapError(e)
	}
	return rows, nil
}
func (w Worker) Complete(ctx context.Context, l outbox.Lease) (bool, error) {
	var accepted bool
	e := w.leaseTx(ctx, l, func(ctx context.Context, q *db.Queries) error {
		var e error
		accepted, e = q.CompleteOutboxLease(ctx, db.CompleteOutboxLeaseParams{TenantID: l.Delivery.TenantID, EventID: l.Delivery.EventID, WorkerID: l.WorkerID, Attempt: l.Attempt})
		return e
	})
	return accepted && e == nil, MapError(e)
}
func (w Worker) Fail(ctx context.Context, l outbox.Lease, code string, retry int32) (bool, error) {
	if code != "OUTBOX_SEND_FAILED" {
		return false, errors.New("OUTBOX_UNSAFE_FAILURE_CODE")
	}
	var accepted bool
	e := w.leaseTx(ctx, l, func(ctx context.Context, q *db.Queries) error {
		var e error
		accepted, e = q.FailOutboxLease(ctx, db.FailOutboxLeaseParams{TenantID: l.Delivery.TenantID, EventID: l.Delivery.EventID, WorkerID: l.WorkerID, Attempt: l.Attempt, ErrorCode: code, RetrySeconds: retry})
		return e
	})
	return accepted && e == nil, MapError(e)
}
