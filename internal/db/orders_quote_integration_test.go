//go:build pgx

// Responsibility: verify public negotiated-order/pre-capture ports with real PG.
// Dependencies: existing rollback-only fixture, Orders public ports, generated SQL.
// Invariants: immutable source hash, exact fixed money, live holds and tenant fences;
// this focused test does not prove quote authority or checkout provider recovery.
package db_test

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func TestOrdersNegotiatedSourceAndReservationPreflight(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Fatal("NOT_RUN: mandatory expendable migrated PostgreSQL DATABASE_URL")
	}
	f := newOrderFixture(t)
	in := f.input(t)
	in.Lines[0].Price.PriceListID = ""
	in.Lines[0].Price.Priority = 0
	in.Lines[0].Price.MinQuantity = 0
	in.Lines[0].QuoteSource = &c.QuoteSource{QuoteID: uuid.NewString(), Version: 4, LineNo: 1}
	creator := orders.NewQuoteCreator()
	validator := orders.NewReservationValidator()
	created, err := creator.CreateFromQuote(f.ctx, f.tx, f.tenant, in)
	if err != nil || created.Total != (money.Money{Amount: 270, Currency: "USD"}) {
		t.Fatalf("negotiated create: %+v %v", created, err)
	}
	replay, err := creator.CreateFromQuote(f.ctx, f.tx, f.tenant, in)
	if err != nil || replay != created {
		t.Fatalf("negotiated replay: %+v %v", replay, err)
	}
	if err := validator.ValidateReservations(f.ctx, f.tx, f.tenant, in.ID); err != nil {
		t.Fatalf("live hold rejected: %v", err)
	}
	f.errorAtSavepoint(t, func() error { return validator.ValidateReservations(f.ctx, f.tx, f.foreign, in.ID) }, c.ErrNotFound)
	f.errorAtSavepoint(t, func() error { return validator.ValidateReservations(f.ctx, f.tx, f.tenant, uuid.NewString()) }, c.ErrNotFound)
	changed := in
	changed.Lines = append([]c.Line(nil), in.Lines...)
	source := *changed.Lines[0].QuoteSource
	source.Version++
	changed.Lines[0].QuoteSource = &source
	f.errorAtSavepoint(t, func() error { _, e := creator.CreateFromQuote(f.ctx, f.tx, f.tenant, changed); return e }, c.ErrKeyReused)
	f.errorAtSavepoint(t, func() error { _, e := orders.New().Create(f.ctx, f.tx, f.tenant, in); return e }, c.ErrInvalid)
	f.errorAtSavepoint(t, func() error {
		if _, e := f.tx.Exec(f.ctx, "RESET ROLE"); e != nil {
			return e
		}
		if _, e := f.tx.Exec(f.ctx, "UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE owner_id=$1", in.ID); e != nil {
			return e
		}
		if _, e := f.tx.Exec(f.ctx, "SET LOCAL ROLE app_runtime"); e != nil {
			return e
		}
		return validator.ValidateReservations(f.ctx, f.tx, f.tenant, in.ID)
	}, c.ErrConflict)
	if _, err := orders.New().Transition(f.ctx, f.tx, f.tenant, c.TransitionInput{OrderID: in.ID, Target: c.Cancelled, EventKey: "negotiated-test-cancel"}); err != nil {
		t.Fatal(err)
	}
	f.errorAtSavepoint(t, func() error { return validator.ValidateReservations(f.ctx, f.tx, f.tenant, in.ID) }, c.ErrConflict)
}
