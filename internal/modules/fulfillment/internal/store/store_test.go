// Responsibilities: keep SQL idempotency conflicts distinct from line conflicts.
// Dependencies: PostgreSQL error metadata and fulfillment public errors.
// Callers: Go gate. Invariant: only the exact app.shipments key constraint is 422.
package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestMapErrorIdempotencyConstraint(t *testing.T) {
	for _, tt := range []struct {
		name, schema, table, constraint string
		want                            error
	}{
		{"creation key", "app", "shipments", "shipments_tenant_id_creation_key_key", contract.ErrKeyReused},
		{"line identity", "app", "shipment_lines", "shipment_lines_tenant_id_shipment_id_order_line_id_key", contract.ErrConflict},
		{"primary key", "app", "shipments", "shipments_pkey", contract.ErrConflict},
		{"same name foreign schema", "other", "shipments", "shipments_tenant_id_creation_key_key", contract.ErrConflict},
		{"same name foreign table", "app", "shipment_lines", "shipments_tenant_id_creation_key_key", contract.ErrConflict},
		{"unknown unique", "app", "shipments", "", contract.ErrConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("controlled command: %w", &pgxadapter.PgError{Code: "23505", SchemaName: tt.schema, TableName: tt.table, ConstraintName: tt.constraint})
			if got := MapError(err); !errors.Is(got, tt.want) {
				t.Fatalf("unique constraint %s classified as %v; want %v", tt.constraint, got, tt.want)
			}
		})
	}
}
