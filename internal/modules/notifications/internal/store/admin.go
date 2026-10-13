// Responsibility: map notifications' generated queries into safe admin DTOs.
// Dependencies: sqlc/pgx; called by private admin service with authorized runner.
// Invariants: no pool/raw DB errors, tenant-scoped reads, immutable redrive receipt.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Admin struct {
	Run func(context.Context, string, func(context.Context, pgx.Tx) error) error
}

func UUID(s string) pgtype.UUID { var u pgtype.UUID; _ = u.Scan(s); return u }
func ID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	s, _ := u.Value()
	return s.(string)
}
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperrors.New(404, "notification_not_found", "Notification not found")
	}
	var p *pgxadapter.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "P0002", "42501":
			return apperrors.New(404, "notification_not_found", "Notification not found")
		case "22023", "22P02":
			return apperrors.New(400, "notification_invalid", "Invalid notification request")
		case "55000", "23505":
			return apperrors.New(409, "notification_conflict", "Notification cannot be redriven")
		}
	}
	return apperrors.New(503, "notification_unavailable", "Notifications unavailable")
}
func (a Admin) ListFailures(ctx context.Context, tenant string, limit int32) ([]contract.Failure, error) {
	if a.Run == nil {
		return nil, MapError(errors.New("unavailable"))
	}
	rows := make([]contract.Failure, 0)
	e := a.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		result, e := db.New(tx).ListNotificationFailures(ctx, db.ListNotificationFailuresParams{TenantID: UUID(tenant), RowLimit: limit})
		if e != nil {
			return e
		}
		for _, r := range result {
			rows = append(rows, contract.Failure{ID: ID(r.ID), EventID: ID(r.SourceEventID), TemplateKey: r.TemplateKey, Status: r.Status, Attempts: r.AttemptCount, Generation: r.Generation, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, SafeError: r.LastError.String})
		}
		return nil
	})
	if e != nil {
		return nil, MapError(e)
	}
	return rows, nil
}
func (a Admin) Redrive(ctx context.Context, tenant, id, key string) (contract.RedriveReceipt, error) {
	var receipt contract.RedriveReceipt
	if a.Run == nil {
		return receipt, MapError(errors.New("unavailable"))
	}
	e := a.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		r, e := db.New(tx).RedriveNotification(ctx, db.RedriveNotificationParams{TenantID: UUID(tenant), NotificationID: UUID(id), IdempotencyKey: key})
		if e != nil {
			return e
		}
		receipt = contract.RedriveReceipt{NotificationID: ID(r.NotificationID), Token: ID(r.RequestToken), Status: "pending", Generation: r.Generation}
		return nil
	})
	if e != nil {
		return contract.RedriveReceipt{}, MapError(e)
	}
	return receipt, nil
}
