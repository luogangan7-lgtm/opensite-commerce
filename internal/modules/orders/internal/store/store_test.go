// Error boundary tests assert public classifications without leaking SQL detail.
package store

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"testing"
)

func TestErrorBoundary(t *testing.T) {
	tests := []struct{ err, want error }{
		{pgx.ErrNoRows, contract.ErrNotFound},
		{errors.New("driver connection failure secret data"), contract.ErrUnavailable},
		{&pgconn.PgError{Code: "XX000", Message: "internal schema details"}, contract.ErrUnavailable},
		{&pgconn.PgError{Code: "P0001", Message: "INVENTORY_INSUFFICIENT_STOCK"}, contract.ErrStockUnavailable},
		{&pgconn.PgError{Code: "P0002", Message: "ORDER_NOT_FOUND"}, contract.ErrNotFound},
		{&pgconn.PgError{Code: "42501", Message: "ORDER_RESERVATION_OWNER_MISMATCH"}, contract.ErrReservationOwner},
		{&pgconn.PgError{Code: "22023", Message: "ORDER_IDEMPOTENCY_KEY_REUSED"}, contract.ErrKeyReused},
		{&pgconn.PgError{Code: "22023", Message: "ORDER_EVENT_KEY_REUSED"}, contract.ErrKeyReused},
		{&pgconn.PgError{Code: "42501", Message: "ORDERS_TENANT_CONTEXT_REQUIRED"}, contract.ErrNotFound},
		{&pgconn.PgError{Code: "22003", Message: "amount overflow"}, contract.ErrInvalid},
		{&pgconn.PgError{Code: "55000", Message: "ORDER_INVALID_TRANSITION"}, contract.ErrConflict},
		{&pgconn.PgError{Code: "40001", Message: "could not serialize"}, contract.ErrRetry},
		{&pgconn.PgError{Code: "40P01", Message: "deadlock detected"}, contract.ErrRetry},
		{&pgconn.PgError{Code: "23505", Message: "duplicate key"}, contract.ErrConflict},
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
