// Responsibility: staff failure listing and explicit idempotent redrive.
// Dependencies: contract/tenant validation; platform transport maps safe errors.
// Invariants: limit/key admission before DB, no uncertain replay admission here.
package service

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"strings"
)

type AdminRepository interface {
	ListFailures(context.Context, string, int32) ([]contract.Failure, error)
	Redrive(context.Context, string, string, string) (contract.RedriveReceipt, error)
}
type Admin struct{ Repo AdminRepository }

func (a Admin) ListFailures(ctx context.Context, tenant string, limit int32) ([]contract.Failure, error) {
	if !tenanttx.ValidTenantID(tenant) || limit < 1 || limit > 100 {
		return nil, apperrors.New(400, "notification_invalid", "Invalid notification request")
	}
	if a.Repo == nil {
		return nil, apperrors.New(503, "notification_unavailable", "Notifications unavailable")
	}
	return a.Repo.ListFailures(ctx, tenant, limit)
}
func (a Admin) Redrive(ctx context.Context, tenant, id, key string) (contract.RedriveReceipt, error) {
	if !tenanttx.ValidTenantID(tenant) || !tenanttx.ValidTenantID(id) || !validKey(key) {
		return contract.RedriveReceipt{}, apperrors.New(400, "notification_invalid", "Invalid notification request")
	}
	if a.Repo == nil {
		return contract.RedriveReceipt{}, apperrors.New(503, "notification_unavailable", "Notifications unavailable")
	}
	return a.Repo.Redrive(ctx, tenant, id, key)
}
func validKey(s string) bool {
	if len(s) < 1 || len(s) > 128 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
