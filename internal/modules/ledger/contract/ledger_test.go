// Responsibility: verify the public exact posting invariants without database I/O.
// Dependencies: ledger contract, money and stdlib; caller: module unit gate.
// Invariant: bad input never reaches SQL, and ordered input is never mutated.
package contract_test

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func valid() contract.Transaction {
	return contract.Transaction{TenantID: "00000000-0000-0000-0000-000000000001", Currency: "USD", SourceType: "order", SourceID: "order-1", Event: "captured", Entries: []contract.Entry{
		{AccountID: "00000000-0000-0000-0000-000000000002", Side: contract.Debit, Amount: money.Money{Amount: 100, Currency: "USD"}},
		{AccountID: "00000000-0000-0000-0000-000000000003", Side: contract.Credit, Amount: money.Money{Amount: 100, Currency: "USD"}},
	}}
}
func TestTransactionValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*contract.Transaction)
		want   error
	}{
		{"valid", func(*contract.Transaction) {}, nil},
		{"bad tenant", func(x *contract.Transaction) { x.TenantID = "bad" }, contract.ErrInvalid},
		{"zero tenant", func(x *contract.Transaction) { x.TenantID = "00000000-0000-0000-0000-000000000000" }, contract.ErrInvalid},
		{"bad account", func(x *contract.Transaction) { x.Entries[0].AccountID = "bad" }, contract.ErrInvalid},
		{"lower currency", func(x *contract.Transaction) { x.Currency = "usd" }, contract.ErrInvalid},
		{"currency too long", func(x *contract.Transaction) { x.Currency = "USDx" }, contract.ErrInvalid},
		{"currency mismatch", func(x *contract.Transaction) { x.Entries[0].Amount.Currency = "EUR" }, contract.ErrInvalid},
		{"zero", func(x *contract.Transaction) { x.Entries[0].Amount.Amount = 0 }, contract.ErrInvalid},
		{"negative", func(x *contract.Transaction) { x.Entries[0].Amount.Amount = -1 }, contract.ErrInvalid},
		{"invalid side", func(x *contract.Transaction) { x.Entries[0].Side = "Debit" }, contract.ErrInvalid},
		{"imbalance", func(x *contract.Transaction) { x.Entries[1].Amount.Amount = 99 }, contract.ErrInvalid},
		{"empty", func(x *contract.Transaction) { x.Entries = nil }, contract.ErrInvalid},
		{"one", func(x *contract.Transaction) { x.Entries = x.Entries[:1] }, contract.ErrInvalid},
		{"1001", func(x *contract.Transaction) { x.Entries = make([]contract.Entry, 1001) }, contract.ErrInvalid},
		{"empty type", func(x *contract.Transaction) { x.SourceType = " " }, contract.ErrInvalid},
		{"empty id", func(x *contract.Transaction) { x.SourceID = "" }, contract.ErrInvalid},
		{"long type", func(x *contract.Transaction) { x.SourceType = strings.Repeat("x", 129) }, contract.ErrInvalid},
		{"long id", func(x *contract.Transaction) { x.SourceID = strings.Repeat("x", 257) }, contract.ErrInvalid},
		{"long event", func(x *contract.Transaction) { x.Event = strings.Repeat("x", 129) }, contract.ErrInvalid},
		{"nul event", func(x *contract.Transaction) { x.Event = "a\x00b" }, contract.ErrInvalid},
		{"bad UTF8", func(x *contract.Transaction) { x.Event = string([]byte{255}) }, contract.ErrInvalid},
		{"1000 entries", func(x *contract.Transaction) {
			pair := append([]contract.Entry(nil), x.Entries...)
			for i := 1; i < 500; i++ {
				x.Entries = append(x.Entries, pair...)
			}
		}, nil},
		{"empty event", func(x *contract.Transaction) { x.Event = "" }, contract.ErrInvalid},
		{"overflow", func(x *contract.Transaction) {
			x.Entries[0].Amount.Amount = math.MaxInt64
			x.Entries[1].Amount.Amount = math.MaxInt64
			x.Entries = append(x.Entries, x.Entries...)
		}, contract.ErrOverflow},
		{"max amount", func(x *contract.Transaction) {
			x.Entries[0].Amount.Amount = math.MaxInt64
			x.Entries[1].Amount.Amount = math.MaxInt64
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := valid()
			c.change(&x)
			before := x
			before.Entries = append([]contract.Entry(nil), x.Entries...)
			if err := x.Validate(); !errors.Is(err, c.want) {
				t.Fatalf("got %v; want %v", err, c.want)
			}
			if !reflect.DeepEqual(x, before) {
				t.Fatal("validation mutated input")
			}
		})
	}
}
