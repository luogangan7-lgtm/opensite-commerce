// Order snapshot invariants use independent arithmetic and mutation fixtures.
package service

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

const orderID = "11111111-1111-4111-8111-111111111111"
const lineID = "22222222-2222-4222-8222-222222222222"
const skuID = "33333333-3333-4333-8333-333333333333"

func input() contract.CreateInput {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return contract.CreateInput{ID: orderID, IdempotencyKey: "order:test", Currency: "USD", Buyer: contract.Buyer{Guest: contract.Contact{Email: "buyer@example.test"}},
		Lines: []contract.Line{{ID: lineID, SKUID: skuID, SKU: "SKU-1", Quantity: 2, Price: pricing.Result{Unit: money.Money{Amount: 100, Currency: "USD"}, Line: money.Money{Amount: 200, Currency: "USD"}, PriceListID: skuID, MinQuantity: 1},
			Tax: tax.LineResult{ID: lineID, Amounts: tax.Amounts{Net: 200, Tax: 20, Gross: 220}, Snapshot: tax.Snapshot{Currency: "USD", ZoneID: skuID, RateID: skuID, Category: "standard", BasisPoints: 1000, ValidFrom: &start, Rounding: "half_up", Reason: "rate"}}}},
		Shipping: shipping.Result{Charge: money.Money{Amount: 50, Currency: "USD"}, ZoneID: skuID, RateID: skuID, Method: "fixed"}}
}
func TestRejectSnapshotMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*contract.CreateInput)
	}{
		{"invalid currency", func(x *contract.CreateInput) { x.Currency = "usd" }},
		{"quantity zero", func(x *contract.CreateInput) { x.Lines[0].Quantity = 0 }},
		{"negative price", func(x *contract.CreateInput) { x.Lines[0].Price.Unit.Amount = -1 }},
		{"duplicate reservation", func(x *contract.CreateInput) {
			x.Lines[0].ReservationID = skuID
			next := x.Lines[0]
			next.ID = skuID
			next.Tax.ID = skuID
			x.Lines = append(x.Lines, next)
		}},
		{"tax rate too high", func(x *contract.CreateInput) { x.Lines[0].Tax.Snapshot.BasisPoints = 10001 }},
		{"tax rounding unknown", func(x *contract.CreateInput) { x.Lines[0].Tax.Snapshot.Rounding = "down" }},
		{"tax window invalid", func(x *contract.CreateInput) {
			start := *x.Lines[0].Tax.Snapshot.ValidFrom
			x.Lines[0].Tax.Snapshot.ValidUntil = &start
		}},
		{"tax time JSON invalid", func(x *contract.CreateInput) {
			start := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			x.Lines[0].Tax.Snapshot.ValidFrom = &start
		}},
		{"exempt nonzero tax", func(x *contract.CreateInput) {
			x.Lines[0].Tax.Snapshot = tax.Snapshot{Currency: "USD", ZoneID: skuID, Category: "exempt", Reason: "exempt", Rounding: "half_up"}
		}},
		{"zero discount", func(x *contract.CreateInput) {
			x.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "discount", AmountMinor: 0, Reason: "x"}}
		}},
		{"positive discount", func(x *contract.CreateInput) {
			x.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "discount", AmountMinor: 1, Reason: "x"}}
		}},
		{"negative surcharge", func(x *contract.CreateInput) {
			x.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "surcharge", AmountMinor: -1, Reason: "x"}}
		}},
		{"invalid key chars", func(x *contract.CreateInput) { x.IdempotencyKey = "key\n" }},
		{"long ttl", func(x *contract.CreateInput) { x.ReservationTTLSeconds = 86401 }},
		{"currency", func(x *contract.CreateInput) { x.Shipping.Charge.Currency = "EUR" }},
		{"quantity price mismatch", func(x *contract.CreateInput) { x.Lines[0].Quantity = 3 }},
		{"tax price mismatch", func(x *contract.CreateInput) { x.Lines[0].Tax.Amounts = tax.Amounts{Net: 100, Tax: 10, Gross: 110} }},
		{"tax rate amount mismatch", func(x *contract.CreateInput) { x.Lines[0].Tax.Amounts = tax.Amounts{Net: 200, Tax: 21, Gross: 221} }},
		{"tax total mismatch", func(x *contract.CreateInput) { x.Lines[0].Tax.Amounts.Gross = 219 }},
		{"tax line identity", func(x *contract.CreateInput) { x.Lines[0].Tax.ID = skuID }},
		{"tax currency", func(x *contract.CreateInput) { x.Lines[0].Tax.Snapshot.Currency = "EUR" }},
		{"missing tax", func(x *contract.CreateInput) { x.Lines[0].Tax.Snapshot = tax.Snapshot{} }},
		{"negative shipping", func(x *contract.CreateInput) { x.Shipping.Charge.Amount = -1 }},
		{"missing shipping quote", func(x *contract.CreateInput) { x.Shipping.RateID = "" }},
		{"duplicate line", func(x *contract.CreateInput) { x.Lines = append(x.Lines, x.Lines[0]) }},
		{"no buyer", func(x *contract.CreateInput) { x.Buyer = contract.Buyer{} }},
		{"two buyers", func(x *contract.CreateInput) { x.Buyer.CustomerID = skuID }},
		{"invalid guest email", func(x *contract.CreateInput) { x.Buyer.Guest.Email = "garbage" }},
		{"no lines", func(x *contract.CreateInput) { x.Lines = nil }},
		{"bad UUID", func(x *contract.CreateInput) { x.ID = "bad" }},
		{"key blank", func(x *contract.CreateInput) { x.IdempotencyKey = " " }},
		{"negative ttl", func(x *contract.CreateInput) { x.ReservationTTLSeconds = -1 }},
		{"adjustment over-discount", func(x *contract.CreateInput) {
			x.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "discount", AmountMinor: -1000, Reason: "coupon"}}
		}},
		{"adjustment bad kind", func(x *contract.CreateInput) {
			x.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "weird", AmountMinor: 1, Reason: "x"}}
		}},
		{"overflow multiply", func(x *contract.CreateInput) { x.Lines[0].Price.Unit.Amount = math.MaxInt64 }},
		{"overflow total", func(x *contract.CreateInput) { x.Shipping.Charge.Amount = math.MaxInt64 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input()
			tt.mutate(&in)
			if _, err := prepare(in); err == nil {
				t.Fatalf("accepted invalid snapshot: %s", tt.name)
			}
		})
	}
}
func TestSnapshotCopiesAndConserves(t *testing.T) {
	in := input()
	in.Adjustments = []contract.Adjustment{{ID: skuID, Kind: "discount", AmountMinor: -10, Reason: "coupon"}}
	p, err := prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.total != 260 || p.ttl != 900 {
		t.Fatalf("totals=%d ttl=%d", p.total, p.ttl)
	}
	var lines []map[string]any
	if err = json.Unmarshal(p.lines, &lines); err != nil {
		t.Fatal(err)
	}
	in.Lines[0].SKU = "mutated"
	in.Lines[0].Tax.Snapshot.ValidFrom = new(time.Time)
	var again []map[string]any
	if err = json.Unmarshal(p.lines, &again); err != nil {
		t.Fatal(err)
	}
	if again[0]["sku"] != "SKU-1" || again[0]["line_total_minor"] != float64(220) {
		t.Fatalf("snapshot drift: %v", again)
	}
}
func TestInclusiveTaxPriceBinding(t *testing.T) {
	in := input()
	in.Lines[0].Price.Unit.Amount = 110
	in.Lines[0].Price.Line.Amount = 220
	in.Lines[0].Tax.Snapshot.IncludeTax = true
	p, err := prepare(in)
	if err != nil || p.total != 270 {
		t.Fatalf("inclusive tax: %+v %v", p, err)
	}
}

func TestRequestHashBindsSnapshot(t *testing.T) {
	in := input()
	p, err := prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	first, err := requestHash(in, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := requestHash(in, p)
	if err != nil || !bytes.Equal(first, second) || len(first) != 32 {
		t.Fatalf("unstable request hash: %x/%x %v", first, second, err)
	}
	changed := input()
	changed.Shipping.Charge.Amount++
	cp, err := prepare(changed)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := requestHash(changed, cp)
	if err != nil || bytes.Equal(first, ch) {
		t.Fatalf("shipping not bound: %v", err)
	}
	changed = input()
	changed.Lines[0].ReservationID = skuID
	cp, err = prepare(changed)
	if err != nil {
		t.Fatal(err)
	}
	ch, err = requestHash(changed, cp)
	if err != nil || bytes.Equal(first, ch) {
		t.Fatalf("explicit reservation not bound: %v", err)
	}
}

func TestTaxSnapshotTimezoneNormalization(t *testing.T) {
	in := input()
	p, err := prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	first, err := requestHash(in, p)
	if err != nil {
		t.Fatal(err)
	}
	offset := in.Lines[0].Tax.Snapshot.ValidFrom.In(time.FixedZone("offset", 8*3600+12))
	in.Lines[0].Tax.Snapshot.ValidFrom = &offset
	p, err = prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := requestHash(in, p)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("same instant snapshot differs: %x/%x %v", first, second, err)
	}
}
func TestExemptSnapshot(t *testing.T) {
	in := input()
	in.Lines[0].Tax.Amounts = tax.Amounts{Net: 200, Gross: 200}
	in.Lines[0].Tax.Snapshot = tax.Snapshot{Currency: "USD", ZoneID: skuID, Category: "exempt", Reason: "exempt", Rounding: "half_up"}
	p, err := prepare(in)
	if err != nil || p.total != 250 {
		t.Fatalf("exemption: %+v %v", p, err)
	}
}

func TestReservationLocksCanonicalOrder(t *testing.T) {
	lines := []lineSnapshot{{SKUID: "b", ID: "b"}, {SKUID: "a", ID: "b"}, {SKUID: "a", ID: "a"}}
	before := append([]lineSnapshot(nil), lines...)
	got := reservationOrder(lines)
	if !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("lock order=%v; want SKU then line identity", got)
	}
	if !reflect.DeepEqual(lines, before) {
		t.Fatal("reservation sort mutated snapshot order")
	}
}
