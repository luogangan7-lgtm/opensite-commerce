// Package tenanttx provides a driver-neutral, transaction-scoped tenant runner.
// RLS's app.tenant_id is set by the driver before *any* domain query runs.
// It is not a security boundary against callers who can submit arbitrary SQL.
package tenanttx

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Tx is the minimum lifecycle contract required of a transaction.
// pgx.Tx directly satisfies it; no external package is needed here.
type Tx interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

type Begin[T Tx] func(context.Context) (T, error)
type SetTenant[T Tx] func(context.Context, T, string) error

var ErrInvalidTenant = errors.New("tenant ID must be a canonical UUID")

// ValidTenantID rejects ambiguous/zero IDs before opening a transaction.
// It does not authenticate or authorize the tenant; the API must do that first.
func ValidTenantID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch i {
		case 8, 13, 18, 23:
			if id[i] != '-' {
				return false
			}
		default:
			c := id[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

// Run executes fn inside a single scoped transaction.
// The caller must pass an authenticated and authorized tenant ID. It must not
// supply arbitrary SQL or a transaction begun outside this runner to fn.
// Rollback on every non-committed exit is guaranteed, including panic paths.
// A bounded, cancellation-independent context ensures cleanup is attempted
// even when the HTTP request was canceled.
func Run[T Tx](ctx context.Context, tenantID string, begin Begin[T], setTenant SetTenant[T], fn func(context.Context, T) error) error {
	if !ValidTenantID(tenantID) {
		return ErrInvalidTenant
	}
	if begin == nil || setTenant == nil || fn == nil {
		return errors.New("transaction dependencies cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanupCtx)
		}
	}()
	if err := setTenant(ctx, tx, tenantID); err != nil {
		return fmt.Errorf("scope tenant transaction: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tenant transaction: %w", err)
	}
	committed = true
	return nil
}
