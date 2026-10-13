// Responsibility: transaction admission, lock ordering and exact actor restoration
// regressions. Dependencies: a tx spy around generated SQL; real RLS belongs to PG.
// Invariants: no pool/transaction acquisition, no price/stock projection queries.
package store

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

const intentTenant = "10000000-0000-4000-8000-000000000001"
const intentID = "10000000-0000-4000-8000-000000000002"
const intentSKU = "10000000-0000-4000-8000-000000000003"
const intentCustomer = "10000000-0000-4000-8000-000000000004"

type intentRow struct {
	values []any
	err    error
}

func (r intentRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected scan shape")
	}
	for i, v := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

type intentRows struct {
	pgx.Rows
	pos int
}

func (r *intentRows) Next() bool { r.pos++; return r.pos == 1 }
func (r *intentRows) Scan(dest ...any) error {
	return (intentRow{values: []any{uuid(intentTenant), uuid(intentID), uuid(intentSKU), int64(2), uuid(intentSKU), "ignored-sku-code", true}}).Scan(dest...)
}
func (*intentRows) Err() error { return nil }
func (*intentRows) Close()     {}

type intentTx struct {
	pgx.Tx
	actor                          contract.AuthorizedActor
	tenant, role                   string
	calls                          []string
	contexts                       [][]any
	getErr, linesErr, errorRestore error
}

func (tx *intentTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(query, "name: GetCartContext"):
		tx.calls = append(tx.calls, "context")
		return intentRow{values: []any{tx.tenant, "prior-customer", "prior-token", tx.role}}
	case strings.Contains(query, "name: GetCartForUpdate"):
		tx.calls = append(tx.calls, "lock")
		if !strings.Contains(query, "FOR UPDATE") {
			return intentRow{err: errors.New("missing cart lock")}
		}
		return intentRow{values: []any{uuid(intentTenant), uuid(intentID), uuid(tx.actor.CustomerID), tx.actor.GuestTokenHash, "USD", int64(9), pgtype.Timestamptz{}}, err: tx.getErr}
	default:
		panic("unexpected cart intent query: " + query)
	}
}
func (tx *intentTx) Exec(_ context.Context, query string, args ...any) (pgxadapter.CommandTag, error) {
	if !strings.Contains(query, "name: SetCartContext") {
		panic("unexpected write in cart intent")
	}
	tx.contexts = append(tx.contexts, append([]any(nil), args...))
	tx.calls = append(tx.calls, "set")
	if len(tx.contexts) > 1 && tx.errorRestore != nil {
		return pgxadapter.CommandTag{}, tx.errorRestore
	}
	return pgxadapter.NewCommandTag("SELECT 1"), nil
}
func (tx *intentTx) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(query, "name: ListCartLines") {
		panic("price or stock projection was read")
	}
	tx.calls = append(tx.calls, "lines")
	return &intentRows{}, tx.linesErr
}
func intentFixture(customer bool) (*intentTx, contract.AuthorizedActor) {
	a := contract.AuthorizedActor{TenantID: intentTenant, GuestTokenHash: make([]byte, 32)}
	if customer {
		a.CustomerID = intentCustomer
		a.GuestTokenHash = nil
	}
	return &intentTx{actor: a, tenant: intentTenant, role: "app_runtime"}, a
}

func TestReadIntentLocksBeforeLinesAndRestoresContext(t *testing.T) {
	for _, customer := range []bool{false, true} {
		t.Run(map[bool]string{false: "guest", true: "customer"}[customer], func(t *testing.T) {
			tx, a := intentFixture(customer)
			out, err := ReadIntent(context.Background(), tx, a, intentID)
			if err != nil || out.ID != intentID || out.TenantID != intentTenant || out.Version != 9 || len(out.Lines) != 1 || out.Lines[0].SKUID != intentSKU || out.Lines[0].Quantity != 2 {
				t.Fatalf("intent %#v, err %v", out, err)
			}
			if !reflect.DeepEqual(tx.calls, []string{"context", "set", "lock", "lines", "set"}) {
				t.Fatalf("transaction order %v", tx.calls)
			}
			want := [][]any{{intentTenant, a.CustomerID, hex.EncodeToString(a.GuestTokenHash)}, {intentTenant, "prior-customer", "prior-token"}}
			if !reflect.DeepEqual(tx.contexts, want) {
				t.Fatalf("actor contexts %v", tx.contexts)
			}
		})
	}
}
func TestReadIntentRejectsUntrustedTransactionOrActor(t *testing.T) {
	cases := []struct {
		name   string
		change func(*intentTx, *contract.AuthorizedActor)
		nilTx  bool
	}{
		{"nil transaction", func(*intentTx, *contract.AuthorizedActor) {}, true},
		{"missing tenant stamp", func(tx *intentTx, _ *contract.AuthorizedActor) { tx.tenant = "" }, false},
		{"wrong tenant stamp", func(tx *intentTx, _ *contract.AuthorizedActor) { tx.tenant = intentCustomer }, false},
		{"owner role", func(tx *intentTx, _ *contract.AuthorizedActor) { tx.role = "app_migrator" }, false},
		{"missing actor", func(_ *intentTx, a *contract.AuthorizedActor) { a.GuestTokenHash = nil }, false},
		{"short token", func(_ *intentTx, a *contract.AuthorizedActor) { a.GuestTokenHash = make([]byte, 31) }, false},
		{"mixed identity", func(_ *intentTx, a *contract.AuthorizedActor) { a.CustomerID = intentCustomer }, false},
		{"invalid customer", func(_ *intentTx, a *contract.AuthorizedActor) { a.GuestTokenHash = nil; a.CustomerID = "client-input" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, a := intentFixture(false)
			tc.change(tx, &a)
			var supplied pgx.Tx = tx
			if tc.nilTx {
				supplied = nil
			}
			_, err := ReadIntent(context.Background(), supplied, a, intentID)
			if err == nil {
				t.Fatal("untrusted actor or transaction admitted")
			}
			if len(tx.contexts) > 0 {
				t.Fatal("untrusted input changed actor context")
			}
		})
	}
}
func TestReadIntentRestoresOnFailureAndReportsRestoreFailure(t *testing.T) {
	for _, kind := range []string{"missing cart", "line error", "restore error"} {
		t.Run(kind, func(t *testing.T) {
			tx, a := intentFixture(false)
			switch kind {
			case "missing cart":
				tx.getErr = pgx.ErrNoRows
			case "line error":
				tx.linesErr = errors.New("line read failed")
			case "restore error":
				tx.errorRestore = errors.New("restore failed")
			}
			_, err := ReadIntent(context.Background(), tx, a, intentID)
			if err == nil {
				t.Fatal("failure swallowed")
			}
			if len(tx.contexts) != 2 || !reflect.DeepEqual(tx.contexts[1], []any{intentTenant, "prior-customer", "prior-token"}) {
				t.Fatalf("original actor not restored: %v", tx.contexts)
			}
			if kind == "missing cart" && status(err) != 404 {
				t.Fatalf("missing cart error %v", err)
			}
			if kind == "restore error" && status(err) != 503 {
				t.Fatalf("restore failure error %v", err)
			}
		})
	}
}
