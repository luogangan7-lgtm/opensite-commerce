//go:build pgx

// Responsibility: verify H-2-C creation-key precedence via public orders commands.
// Dependencies: pgxadapter maintenance fixture and production sqlc; callers: tagged PG gates.
// Invariants: only adapter opens connections; fixtures rollback; key denial precedes stock locks.
package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func TestOrdersCreationKeyPrecedesReservations(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("NOT_RUN: expendable migrated PostgreSQL DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner, closeRunner, err := pgxadapter.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRunner()
	rollback := errors.New("fixture rollback")
	err = runner.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		f := orderFixture{ctx: ctx, tx: tx}
		var product string
		if e := tx.QueryRow(ctx, "INSERT INTO app.tenants(name) VALUES('h2c-key') RETURNING id::text").Scan(&f.tenant); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, "INSERT INTO app.products(tenant_id,product_code) VALUES($1,'H2C') RETURNING id::text", f.tenant).Scan(&product); e != nil {
			return e
		}
		// tenant/product arguments are captured only after each authoritative INSERT.
		if e := tx.QueryRow(ctx, "INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES($1,$2,'H2C','default') RETURNING id::text", f.tenant, product).Scan(&f.sku); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SELECT app.inventory_adjust($1,30,'h2c stock',$2,'inbound')", f.sku, uuid.NewString()); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); e != nil {
			return e
		}
		svc := orders.New()
		in := f.input(t)
		if _, err := svc.Create(f.ctx, f.tx, f.tenant, in); err != nil {
			t.Fatal(err)
		}

		// A creation replay/conflict is read-only; an attempted stock lock/reserve
		// fails 25006, making ordering observable without scheduler timing.
		if _, e := tx.Exec(ctx, "SET LOCAL transaction_read_only=on"); e != nil {
			return e
		}
		if replay, e := svc.Create(ctx, tx, f.tenant, in); e != nil || replay.ID != in.ID {
			t.Fatalf("read-only replay: %v %v", replay, e)
		}
		for _, change := range []func(*contract.CreateInput){
			func(x *contract.CreateInput) { x.ID = uuid.NewString() },
			func(x *contract.CreateInput) { x.ReservationTTLSeconds = 901 },
			func(x *contract.CreateInput) { x.Lines[0].ReservationID = uuid.NewString() },
		} {
			changed := in
			changed.Lines = append([]contract.Line(nil), in.Lines...)
			change(&changed)
			f.errorAtSavepoint(t, func() error { _, e := svc.Create(f.ctx, f.tx, f.tenant, changed); return e }, contract.ErrKeyReused)
		}
		var n int
		var reserved int64
		if err := f.tx.QueryRow(f.ctx, "SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=$1", f.tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := f.tx.QueryRow(f.ctx, "SELECT reserved FROM app.stock_levels WHERE tenant_id=$1 AND sku_id=$2", f.tenant, f.sku).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		if n != 1 || reserved != 2 {
			t.Fatalf("key rejection leaked reservations: %d %d", n, reserved)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
}
