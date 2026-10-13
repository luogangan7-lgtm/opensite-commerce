// Responsibilities: classify permanent return idempotency collisions precisely.
// Dependencies: PostgreSQL error metadata and returns public errors.
// Callers: Go gate. Invariant: quantity/line identity conflicts remain 409.
package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
)

func TestMapErrorIdempotencyConstraint(t *testing.T) {
	for _, tt := range []struct {
		name, schema, table, constraint string
		want                            error
	}{
		{"creation key", "app", "returns", "returns_tenant_id_creation_key_key", contract.ErrKeyReused},
		{"receive key", "app", "returns", "returns_tenant_id_receive_key_key", contract.ErrKeyReused},
		{"refund key", "app", "returns", "returns_tenant_id_refund_key_key", contract.ErrKeyReused},
		{"line identity", "app", "return_lines", "return_lines_tenant_id_return_id_order_line_id_shipment_line_i_key", contract.ErrConflict},
		{"primary key", "app", "returns", "returns_pkey", contract.ErrConflict},
		{"same name foreign schema", "other", "returns", "returns_tenant_id_refund_key_key", contract.ErrConflict},
		{"same name foreign table", "app", "return_lines", "returns_tenant_id_refund_key_key", contract.ErrConflict},
		{"unknown unique", "app", "returns", "", contract.ErrConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("controlled command: %w", &pgconn.PgError{Code: "23505", SchemaName: tt.schema, TableName: tt.table, ConstraintName: tt.constraint})
			if got := MapError(err); !errors.Is(got, tt.want) {
				t.Fatalf("unique constraint %s classified as %v; want %v", tt.constraint, got, tt.want)
			}
		})
	}
}
