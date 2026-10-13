// Responsibility: exercise the public transaction port's validation/error boundary.
// Dependencies: ledger, contract, pgx and stdlib; caller: module unit gate.
// Invariant: the fake only checks adapter behavior; real SQL/ACL/idempotency is
// covered separately by ledger_pgx_test, never certified by this transaction spy.
package ledger_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"strings"
	"testing"
	"time"
)

const tenantID = "00000000-0000-0000-0000-000000000001"
const accountID = "00000000-0000-0000-0000-000000000002"

func input() contract.Transaction {
	return contract.Transaction{TenantID: tenantID, Currency: "USD", SourceType: "order", SourceID: "1", Event: "captured", Entries: []contract.Entry{{AccountID: accountID, Side: contract.Debit, Amount: money.Money{Amount: 100, Currency: "USD"}}, {AccountID: accountID, Side: contract.Credit, Amount: money.Money{Amount: 100, Currency: "USD"}}}}
}

type txSpy struct {
	pgx.Tx
	queries int
	row     pgx.Row
	args    []any
}

func (x *txSpy) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	x.queries++
	x.args = args
	return x.row
}

type row struct {
	err    error
	values []any
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, v := range r.values {
		switch d := dest[i].(type) {
		case *pgtype.UUID:
			*d = v.(pgtype.UUID)
		case *string:
			*d = v.(string)
		case *int64:
			*d = v.(int64)
		default:
			panic("unexpected generated destination")
		}
	}
	return nil
}
func TestPostValidatesBeforeSQL(t *testing.T) {
	x := input()
	x.Entries[1].Amount.Amount = 1
	tx := &txSpy{}
	if _, err := ledger.New().Post(context.Background(), tx, x); !errors.Is(err, contract.ErrInvalid) {
		t.Fatal(err)
	}
	if tx.queries != 0 {
		t.Fatal("invalid posting reached SQL")
	}
	if _, err := ledger.New().Post(context.Background(), nil, input()); !errors.Is(err, contract.ErrInvalid) {
		t.Fatal(err)
	}
}
func TestPostKeepsOrderedArrays(t *testing.T) {
	var id pgtype.UUID
	_ = id.Scan(tenantID)
	tx := &txSpy{row: row{values: []any{id}}}
	x := input()
	x.Entries[1].AccountID = "00000000-0000-0000-0000-000000000003"
	got, err := ledger.New().Post(context.Background(), tx, x)
	if err != nil || got != tenantID || tx.queries != 1 {
		t.Fatalf("%s %v query=%d", got, err, tx.queries)
	}
	accounts := tx.args[5].([]pgtype.UUID)
	sides := tx.args[6].([]string)
	amounts := tx.args[7].([]int64)
	if accounts[0].String() != accountID || accounts[1].String() != x.Entries[1].AccountID || sides[0] != "debit" || sides[1] != "credit" || amounts[0] != 100 || amounts[1] != 100 {
		t.Fatalf("ordered content changed: %v", tx.args)
	}
}
func TestErrorBoundary(t *testing.T) {
	cases := []struct {
		name     string
		in, want error
	}{
		{"same key different content", &pgxadapter.PgError{Code: "23505", ConstraintName: "ledger_idempotency_content", Message: "secret driver diagnostic"}, contract.ErrConflict},
		{"other unique failure", &pgxadapter.PgError{Code: "23505", ConstraintName: "other", Message: "secret driver diagnostic"}, contract.ErrUnavailable},
		{"overflow", &pgxadapter.PgError{Code: "22003"}, contract.ErrOverflow},
		{"arguments", &pgxadapter.PgError{Code: "22023"}, contract.ErrInvalid},
		{"balance", &pgxadapter.PgError{Code: "23514"}, contract.ErrInvalid},
		{"missing account", &pgxadapter.PgError{Code: "23503"}, contract.ErrNotFound},
		{"tenant denial", &pgxadapter.PgError{Code: "42501"}, contract.ErrForbidden},
		{"serialization", &pgxadapter.PgError{Code: "40001"}, contract.ErrRetry},
		{"deadlock", &pgxadapter.PgError{Code: "40P01"}, contract.ErrRetry},
		{"missing row", pgx.ErrNoRows, contract.ErrNotFound},
		{"unknown", errors.New("secret driver diagnostic"), contract.ErrUnavailable},
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx := &txSpy{row: row{err: c.in}}
			_, err := ledger.New().Post(context.Background(), tx, input())
			if !errors.Is(err, c.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("got %v; want %v", err, c.want)
			}
			if errors.As(err, new(*pgxadapter.PgError)) {
				t.Fatal("driver error escaped")
			}
		})
	}
}
func TestBalanceNormalSide(t *testing.T) {
	for _, kind := range []string{"asset", "expense", "liability", "equity", "revenue"} {
		t.Run(kind, func(t *testing.T) {
			tx := &txSpy{row: row{values: []any{pgtype.UUID{}, kind, "USD", int64(70), int64(100)}}}
			got, err := ledger.New().Balance(context.Background(), tx, tenantID, accountID, time.Now())
			want := int64(30)
			if kind == "asset" || kind == "expense" {
				want = -30
			}
			if err != nil || got.Currency != "USD" || got.NormalNet.Amount != want || got.Debits.Amount != 70 || got.Credits.Amount != 100 || got.NormalNet.Currency != "USD" {
				t.Fatalf("got %+v %v; want net %d", got, err, want)
			}
		})
	}
	tx := &txSpy{}
	if _, err := ledger.New().Balance(context.Background(), tx, tenantID, accountID, time.Time{}); !errors.Is(err, contract.ErrInvalid) || tx.queries != 0 {
		t.Fatalf("zero cutoff reached SQL: %v", err)
	}
}
