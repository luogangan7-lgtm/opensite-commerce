// Negotiated-order tests bind immutable quote provenance and independent money
// expectations; they use no current pricing rules or database test substitute.
package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
)

func quotedInput() contract.CreateInput {
	in := input()
	in.Lines[0].Price.PriceListID = ""
	in.Lines[0].Price.MinQuantity = 0
	in.Lines[0].QuoteSource = &contract.QuoteSource{QuoteID: skuID, Version: 3, LineNo: 1}
	return in
}

func TestNegotiatedSnapshotUsesFrozenMoney(t *testing.T) {
	in := quotedInput()
	p, err := prepareQuoted(in)
	if err != nil {
		t.Fatalf("approved negotiated snapshot rejected: %v", err)
	}
	if p.total != 270 {
		t.Fatalf("negotiated total = %d, want 270", p.total)
	}
	var lines []struct {
		QuoteSource *contract.QuoteSource `json:"quote_source"`
		Unit        int64                 `json:"unit_price_minor"`
		Net         int64                 `json:"line_net_minor"`
		Tax         int64                 `json:"line_tax_minor"`
	}
	if err := json.Unmarshal(p.lines, &lines); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].QuoteSource == nil || *lines[0].QuoteSource != *in.Lines[0].QuoteSource || lines[0].Unit != 100 || lines[0].Net != 200 || lines[0].Tax != 20 {
		t.Fatalf("negotiated snapshot changed: %s", p.lines)
	}
	if _, err := prepare(in); !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("ordinary price-list Create admitted quote: %v", err)
	}
}

func TestNegotiatedSnapshotRejectsUnsafeSources(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*contract.CreateInput)
	}{
		{"missing", func(in *contract.CreateInput) { in.Lines[0].QuoteSource = nil }},
		{"invalid quote", func(in *contract.CreateInput) { in.Lines[0].QuoteSource.QuoteID = "bad" }},
		{"zero version", func(in *contract.CreateInput) { in.Lines[0].QuoteSource.Version = 0 }},
		{"zero line", func(in *contract.CreateInput) { in.Lines[0].QuoteSource.LineNo = 0 }},
		{"price list masquerade", func(in *contract.CreateInput) { in.Lines[0].Price.PriceListID = skuID }},
		{"priority", func(in *contract.CreateInput) { in.Lines[0].Price.Priority = 1 }},
		{"tier", func(in *contract.CreateInput) { in.Lines[0].Price.MinQuantity = 1 }},
		{"missing SKU", func(in *contract.CreateInput) { in.Lines[0].SKUID = "" }},
		{"currency", func(in *contract.CreateInput) { in.Lines[0].Price.Unit.Currency = "EUR" }},
		{"multiply", func(in *contract.CreateInput) { in.Lines[0].Price.Unit.Amount = 101 }},
		{"tax", func(in *contract.CreateInput) { in.Lines[0].Tax.Amounts.Tax = 21; in.Lines[0].Tax.Amounts.Gross = 221 }},
		{"extra adjustment", func(in *contract.CreateInput) {
			in.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "discount", AmountMinor: -1, Reason: "unquoted"}}
		}},
		{"nonpositive payable", func(in *contract.CreateInput) {
			in.Lines[0].Price.Unit.Amount, in.Lines[0].Price.Line.Amount = 0, 0
			in.Lines[0].Tax.Amounts.Net, in.Lines[0].Tax.Amounts.Tax, in.Lines[0].Tax.Amounts.Gross = 0, 0, 0
			in.Shipping.Charge.Amount = 0
		}},
		{"different quote", func(in *contract.CreateInput) { appendQuotedLine(in); in.Lines[1].QuoteSource.QuoteID = orderID }},
		{"different version", func(in *contract.CreateInput) { appendQuotedLine(in); in.Lines[1].QuoteSource.Version++ }},
		{"duplicate source line", func(in *contract.CreateInput) { appendQuotedLine(in); in.Lines[1].QuoteSource.LineNo = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := quotedInput()
			tt.mutate(&in)
			if _, err := prepareQuoted(in); !errors.Is(err, contract.ErrInvalid) {
				t.Fatalf("unsafe negotiated source accepted: %v", err)
			}
		})
	}
	in := input()
	in.Lines[0].QuoteSource = &contract.QuoteSource{QuoteID: skuID, Version: 3, LineNo: 1}
	if _, err := prepare(in); !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("ordinary Create accepted negotiated provenance with valid price list: %v", err)
	}
}

func TestNegotiatedInclusiveSnapshotAndCopy(t *testing.T) {
	in := quotedInput()
	in.Lines[0].Price.Unit.Amount, in.Lines[0].Price.Line.Amount = 110, 220
	in.Lines[0].Tax.Snapshot.IncludeTax = true
	p, err := prepareQuoted(in)
	if err != nil || p.total != 270 {
		t.Fatalf("inclusive negotiated snapshot: %d %v", p.total, err)
	}
	before := append([]byte(nil), p.lines...)
	in.Lines[0].QuoteSource.Version = 100
	if !bytes.Equal(before, p.lines) {
		t.Fatal("prepared quote source aliased mutable caller")
	}
}

func appendQuotedLine(in *contract.CreateInput) {
	next := in.Lines[0]
	next.ID = orderID
	next.Tax.ID = orderID
	source := *next.QuoteSource
	source.LineNo = 2
	next.QuoteSource = &source
	in.Lines = append(in.Lines, next)
}

func TestNegotiatedRequestHashBindsProvenance(t *testing.T) {
	in := quotedInput()
	p, err := prepareQuoted(in)
	if err != nil {
		t.Fatal(err)
	}
	first, err := requestHash(in, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*contract.QuoteSource)
	}{{"quote", func(q *contract.QuoteSource) { q.QuoteID = orderID }}, {"version", func(q *contract.QuoteSource) { q.Version++ }}, {"line", func(q *contract.QuoteSource) { q.LineNo++ }}} {
		t.Run(tt.name, func(t *testing.T) {
			changed := quotedInput()
			tt.mutate(changed.Lines[0].QuoteSource)
			cp, err := prepareQuoted(changed)
			if err != nil {
				t.Fatal(err)
			}
			h, err := requestHash(changed, cp)
			if err != nil || bytes.Equal(first, h) {
				t.Fatalf("quote %s unbound: %v", tt.name, err)
			}
		})
	}
}
