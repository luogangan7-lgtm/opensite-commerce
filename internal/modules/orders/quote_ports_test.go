// Responsibility: public Orders quote/preflight admission and sanitized errors.
// Dependencies: public ports and pgx test spy; called by focused module tests.
// Invariant: spy evidence covers adapter checks only, never real SQL/stock safety.
package orders_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const order = "22222222-2222-4222-8222-222222222222"

type validationTx struct {
	pgx.Tx
	active       string
	reads, execs int
	err          error
}
type tenantRow string

func (r tenantRow) Scan(dest ...any) error { return dest[0].(*pgtype.UUID).Scan(string(r)) }
func (x *validationTx) QueryRow(context.Context, string, ...any) pgx.Row {
	x.reads++
	return tenantRow(x.active)
}
func (x *validationTx) Exec(context.Context, string, ...any) (pgxadapter.CommandTag, error) {
	x.execs++
	return pgxadapter.CommandTag{}, x.err
}

func TestOrdersLiveReservationFenceAdmission(t *testing.T) {
	svc := orders.New()
	ctx := context.Background()
	x := &validationTx{active: tenant}
	if err := svc.LockLiveReservations(ctx, x, tenant, "bad", 0); !errors.Is(err, contract.ErrInvalid) || x.reads != 0 || x.execs != 0 {
		t.Fatalf("malformed order reached SQL: %v %+v", err, x)
	}
	if err := svc.LockLiveReservations(ctx, nil, tenant, order, 0); !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("nil transaction: %v", err)
	}
	x = &validationTx{active: order}
	if err := svc.LockLiveReservations(ctx, x, tenant, order, 0); !errors.Is(err, contract.ErrNotFound) || x.execs != 0 {
		t.Fatalf("foreign tenant reached preflight: %v %+v", err, x)
	}

}

func TestQuoteCreatorRejectsMalformedCommand(t *testing.T) {
	if _, err := orders.NewQuoteCreator().CreateFromQuote(context.Background(), nil, tenant, contract.CreateInput{}); !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("malformed quote admitted: %v", err)
	}
}
