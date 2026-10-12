// These tests pin tier eligibility and explainable tie order without database I/O.
// Dependencies: money and standard test library; callers are go test/check_all.
package pricing

import (
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func fixture() ([]PriceList, Request) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	request := Request{TenantID: "tenant", SKUID: "sku", Currency: "USD", Quantity: 10, At: at}
	a := PriceList{TenantID: "tenant", ID: "00000000-0000-0000-0000-000000000001", Currency: "USD", Status: "active", StartsAt: at.Add(-time.Hour), Priority: 5, Audience: "all", Entries: []Entry{{"sku", 1, 150}, {"sku", 10, 120}}}
	b := a
	b.ID = "00000000-0000-0000-0000-000000000002"
	b.Entries = []Entry{{"sku", 1, 100}}
	return []PriceList{b, a}, request
}
func TestQuote(t *testing.T) {
	tests := []struct {
		name   string
		change func([]PriceList, *Request)
		amount int64
		id     string
		err    error
	}{
		{"tie and tier", func([]PriceList, *Request) {}, 120, "00000000-0000-0000-0000-000000000001", nil},
		{"priority", func(l []PriceList, r *Request) { l[0].Priority = 6 }, 100, "00000000-0000-0000-0000-000000000002", nil},
		{"small quantity", func(l []PriceList, r *Request) { r.Quantity = 1 }, 150, "00000000-0000-0000-0000-000000000001", nil},
		{"start inclusive", func(l []PriceList, r *Request) { r.At = l[1].StartsAt }, 120, "00000000-0000-0000-0000-000000000001", nil},
		{"end exclusive", func(l []PriceList, r *Request) {
			for i := range l {
				end := r.At
				l[i].EndsAt = &end
			}
		}, 0, "", ErrNoPrice},
		{"future", func(l []PriceList, r *Request) {
			for i := range l {
				l[i].StartsAt = r.At.Add(time.Second)
			}
		}, 0, "", ErrNoPrice},
		{"draft", func(l []PriceList, r *Request) {
			for i := range l {
				l[i].Status = "draft"
			}
		}, 0, "", ErrNoPrice},
		{"foreign tenant", func(l []PriceList, r *Request) { r.TenantID = "other" }, 0, "", ErrNoPrice},
		{"foreign currency", func(l []PriceList, r *Request) { r.Currency = "EUR" }, 0, "", ErrNoPrice},
		{"foreign sku", func(l []PriceList, r *Request) { r.SKUID = "other" }, 0, "", ErrNoPrice},
		{"missing tier", func(l []PriceList, r *Request) { r.Quantity = 0 }, 0, "", ErrInvalid},
		{"group missing", func(l []PriceList, r *Request) {
			for i := range l {
				l[i].Audience = "customer_group"
				l[i].CustomerGroupID = "group"
			}
		}, 0, "", ErrNoPrice},
		{"group matches", func(l []PriceList, r *Request) {
			for i := range l {
				l[i].Audience = "customer_group"
				l[i].CustomerGroupID = "group"
			}
			r.CustomerGroupID = "group"
		}, 120, "00000000-0000-0000-0000-000000000001", nil},
		{"company matches", func(l []PriceList, r *Request) {
			for i := range l {
				l[i].Audience = "buyer_company"
				l[i].BuyerCompanyID = "company"
			}
			r.BuyerCompanyID = "company"
		}, 120, "00000000-0000-0000-0000-000000000001", nil},
		{"overflow", func(l []PriceList, r *Request) { l[1].Entries = []Entry{{"sku", 1, math.MaxInt64}}; r.Quantity = 2 }, 0, "", money.ErrOverflow},
		{"zero price", func(l []PriceList, r *Request) { l[1].Entries = []Entry{{"sku", 1, 0}} }, 0, "00000000-0000-0000-0000-000000000001", nil},
		{"duplicate tier", func(l []PriceList, r *Request) { l[1].Entries = append(l[1].Entries, Entry{"sku", 10, 1}) }, 0, "", ErrInvalid},
		{"negative price", func(l []PriceList, r *Request) { l[1].Entries[0].UnitAmount = -1 }, 0, "", ErrInvalid},
		{"invalid target", func(l []PriceList, r *Request) { l[1].CustomerGroupID = "group" }, 0, "", ErrInvalid},
		{"invalid window", func(l []PriceList, r *Request) { end := l[1].StartsAt; l[1].EndsAt = &end }, 0, "", ErrInvalid},
		{"invalid currency", func(l []PriceList, r *Request) { r.Currency = "usd" }, 0, "", money.ErrInvalidCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lists, r := fixture()
			tc.change(lists, &r)
			got, err := Quote(lists, r)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err=%v want %v", err, tc.err)
			}
			if err == nil && (got.Unit.Amount != tc.amount || got.Line.Amount != tc.amount*r.Quantity || got.PriceListID != tc.id || got.Unit.Currency != r.Currency) {
				t.Fatalf("result=%+v", got)
			}
		})
	}
}
func TestQuoteOrderIndependent(t *testing.T) {
	lists, r := fixture()
	want, err := Quote(lists, r)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1012))
	for i := 0; i < 200; i++ {
		rng.Shuffle(len(lists), func(i, j int) { lists[i], lists[j] = lists[j], lists[i] })
		for j := range lists {
			rng.Shuffle(len(lists[j].Entries), func(a, b int) { lists[j].Entries[a], lists[j].Entries[b] = lists[j].Entries[b], lists[j].Entries[a] })
		}
		before := make([]PriceList, len(lists))
		copy(before, lists)
		for j := range before {
			before[j].Entries = append([]Entry(nil), lists[j].Entries...)
		}
		got, err := Quote(lists, r)
		if err != nil || got != want || !reflect.DeepEqual(lists, before) {
			t.Fatalf("order affected result: %+v %v", got, err)
		}
	}
}
