// Error boundary tests assert public classifications without leaking SQL detail.
package store

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"testing"
)

func TestErrorBoundary(t *testing.T) {
	tests := []struct{ err, want error }{
		{pgx.ErrNoRows, contract.ErrNotFound},
		{errors.New("driver connection failure secret data"), contract.ErrUnavailable},
		{&pgxadapter.PgError{Code: "XX000", Message: "internal schema details"}, contract.ErrUnavailable},
		{&pgxadapter.PgError{Code: "P0001", Message: "INVENTORY_INSUFFICIENT_STOCK"}, contract.ErrStockUnavailable},
		{&pgxadapter.PgError{Code: "P0002", Message: "ORDER_NOT_FOUND"}, contract.ErrNotFound},
		{&pgxadapter.PgError{Code: "42501", Message: "ORDER_RESERVATION_OWNER_MISMATCH"}, contract.ErrReservationOwner},
		{&pgxadapter.PgError{Code: "22023", Message: "ORDER_IDEMPOTENCY_KEY_REUSED"}, contract.ErrKeyReused},
		{&pgxadapter.PgError{Code: "22023", Message: "ORDER_EVENT_KEY_REUSED"}, contract.ErrKeyReused},
		{&pgxadapter.PgError{Code: "42501", Message: "ORDERS_TENANT_CONTEXT_REQUIRED"}, contract.ErrNotFound},
		{&pgxadapter.PgError{Code: "22003", Message: "amount overflow"}, contract.ErrInvalid},
		{&pgxadapter.PgError{Code: "55000", Message: "ORDER_INVALID_TRANSITION"}, contract.ErrConflict},
		{&pgxadapter.PgError{Code: "40001", Message: "could not serialize"}, contract.ErrRetry},
		{&pgxadapter.PgError{Code: "40P01", Message: "deadlock detected"}, contract.ErrRetry},
		{&pgxadapter.PgError{Code: "23505", Message: "duplicate key"}, contract.ErrConflict},
		{&pgxadapter.PgError{Code: "23505", Message: "duplicate key", ConstraintName: "orders_pkey"}, contract.ErrConflict},
		// RR/Serializable: concurrent identical creates both miss the key lookup.
		{&pgxadapter.PgError{Code: "23505", Message: "duplicate key", ConstraintName: "orders_creation_key_unique"}, contract.ErrRetry},
	}
	for _, x := range tests {
		if err := MapError(x.err); !errors.Is(err, x.want) {
			t.Fatalf("%v: got %v want %v", x.err, err, x.want)
		}
		if x.want == contract.ErrRetry && errors.Is(MapError(x.err), contract.ErrConflict) {
			t.Fatalf("%v: retryable abort classified as business conflict", x.err)
		}
	}
}
