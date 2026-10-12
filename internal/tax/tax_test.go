// Contract tests for pure tax: independent examples, integer oracle, config
// rejection, and snapshot isolation. Dependencies: stdlib; caller: go test.
package tax_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var end = start.Add(24 * time.Hour)
var dest = tax.Destination{Country: "US", Subdivision: "US-CA"}

func rules(bp int64) tax.Ruleset {
	u := end
	return tax.Ruleset{
		Zones:      []tax.Zone{{ID: "us", Locations: []tax.Destination{{Country: "US"}}}},
		Categories: []tax.Category{{Code: "standard"}, {Code: "exempt", IsExempt: true}},
		Rates:      []tax.Rate{{ID: "rate", ZoneID: "us", Category: "standard", BasisPoints: bp, ValidFrom: start, ValidUntil: &u}},
	}
}
func item(amount, qty int64) []tax.LineItem {
	return []tax.LineItem{{ID: "line", Category: "standard", UnitAmount: amount, Quantity: qty}}
}
func check(t *testing.T, got tax.Result, err error, want tax.Amounts) {
	t.Helper()
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if got.Totals != want {
		t.Fatalf("totals=%+v want=%+v", got.Totals, want)
	}
}

func TestHalfUpAndLineRounding(t *testing.T) {
	for _, tc := range []struct {
		name            string
		amount, qty, bp int64
		inclusive       bool
		want            tax.Amounts
	}{
		{"exclusive tie", 1, 1, 5000, false, tax.Amounts{Net: 1, Tax: 1, Gross: 2}},
		{"exclusive below", 1, 1, 4999, false, tax.Amounts{Net: 1, Tax: 0, Gross: 1}},
		{"exclusive above", 1, 1, 5001, false, tax.Amounts{Net: 1, Tax: 1, Gross: 2}},
		{"inclusive tie", 3, 1, 2000, true, tax.Amounts{Net: 2, Tax: 1, Gross: 3}},
		{"inclusive below", 2, 1, 2000, true, tax.Amounts{Net: 2, Tax: 0, Gross: 2}},
		{"quantity before rounding", 1, 3, 2000, false, tax.Amounts{Net: 3, Tax: 1, Gross: 4}},
		{"rate zero", 101, 2, 0, false, tax.Amounts{Net: 202, Tax: 0, Gross: 202}},
		{"rate max", 101, 1, 10000, false, tax.Amounts{Net: 101, Tax: 101, Gross: 202}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tax.Calculate(dest, item(tc.amount, tc.qty), tc.inclusive, start, rules(tc.bp))
			check(t, got, err, tc.want)
		})
	}
	items := append(item(1, 1), item(1, 1)...)
	got, err := tax.Calculate(dest, items, false, start, rules(5000))
	check(t, got, err, tax.Amounts{Net: 2, Tax: 2, Gross: 4}) // aggregate round would be tax=1
}

func TestRateWindowsAndSelection(t *testing.T) {
	r := rules(2000)
	r.Rates = append(r.Rates, tax.Rate{ID: "next", ZoneID: "us", Category: "standard", BasisPoints: 1000, ValidFrom: end})
	for _, tc := range []struct {
		at      time.Time
		wantID  string
		wantErr error
	}{
		{start.Add(-time.Nanosecond), "", tax.ErrRateNotFound},
		{start, "rate", nil}, {end.Add(-time.Nanosecond), "rate", nil}, {end, "next", nil},
	} {
		got, err := tax.Calculate(dest, item(100, 1), false, tc.at, r)
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("at %v err=%v want=%v", tc.at, err, tc.wantErr)
		}
		if err == nil && got.Lines[0].Snapshot.RateID != tc.wantID {
			t.Fatalf("at %v snapshot=%+v", tc.at, got.Lines[0].Snapshot)
		}
	}
	r.Zones = append(r.Zones, tax.Zone{ID: "ca", Locations: []tax.Destination{dest}})
	_, err := tax.Calculate(dest, item(100, 1), false, start, r)
	if !errors.Is(err, tax.ErrRateNotFound) {
		t.Fatalf("specific zone must not fall back: %v", err)
	}
	r.Rates = append(r.Rates, tax.Rate{ID: "ca-rate", ZoneID: "ca", Category: "standard", BasisPoints: 750, ValidFrom: start})
	got, err := tax.Calculate(dest, item(100, 1), false, start, r)
	check(t, got, err, tax.Amounts{Net: 100, Tax: 8, Gross: 108})
	if got.Lines[0].Snapshot.ZoneID != "ca" {
		t.Fatal("specific zone did not win")
	}
	r.Zones[0], r.Zones[1] = r.Zones[1], r.Zones[0]
	reversed, err := tax.Calculate(dest, item(100, 1), false, start, r)
	if err != nil || !reflect.DeepEqual(got, reversed) {
		t.Fatalf("selection depends on zone order: %v", err)
	}
	_, err = tax.Calculate(tax.Destination{Country: "US", Subdivision: "US-NY"}, item(100, 1), false, start, r)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExemptAndSnapshotIsolation(t *testing.T) {
	r := rules(2000)
	items := item(123, 2)
	items[0].Category = "exempt"
	r.Rates = nil
	got, err := tax.Calculate(dest, items, true, start, r)
	check(t, got, err, tax.Amounts{Net: 246, Tax: 0, Gross: 246})
	s := got.Lines[0].Snapshot
	if s.Reason != "exempt" || s.BasisPoints != 0 || s.RateID != "" || s.ValidFrom != nil || s.ValidUntil != nil || s.Rounding != "half_up" || !s.IncludeTax {
		t.Fatalf("exempt snapshot=%+v", s)
	}
	r = rules(2000)
	got, err = tax.Calculate(dest, item(100, 1), false, start, r)
	if err != nil {
		t.Fatal(err)
	}
	s = got.Lines[0].Snapshot
	if s.ZoneID != "us" || s.RateID != "rate" || s.Category != "standard" || s.BasisPoints != 2000 || s.ValidFrom == nil || !s.ValidFrom.Equal(start) || s.ValidUntil == nil || !s.ValidUntil.Equal(end) || s.IncludeTax || s.Reason != "rate" || s.Rounding != "half_up" {
		t.Fatalf("rate snapshot=%+v", s)
	}
	before, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	*r.Rates[0].ValidUntil = end.Add(time.Hour)
	r.Rates[0].ValidFrom = end
	r.Rates[0].ID = "changed"
	r.Categories[0].Code = "changed"
	r.Zones[0].ID = "changed"
	after, err := json.Marshal(got)
	if err != nil || string(before) != string(after) {
		t.Fatalf("snapshot aliases input: %s %s %v", before, after, err)
	}
	var roundtrip tax.Result
	if err := json.Unmarshal(before, &roundtrip); err != nil || !reflect.DeepEqual(got, roundtrip) {
		t.Fatalf("JSON roundtrip: %v", err)
	}
	items = append(item(100, 1), item(100, 1)...)
	got, err = tax.Calculate(dest, items, false, start, rules(2000))
	if err != nil {
		t.Fatal(err)
	}
	*got.Lines[0].Snapshot.ValidUntil = start
	if !got.Lines[1].Snapshot.ValidUntil.Equal(end) {
		t.Fatal("snapshots alias sibling lines")
	}
}

func TestExemptIgnoresConfiguredRate(t *testing.T) {
	r := rules(2000)
	r.Rates = append(r.Rates, tax.Rate{ID: "exempt-rate", ZoneID: "us", Category: "exempt", BasisPoints: 2000, ValidFrom: start})
	items := item(100, 1)
	items[0].Category = "exempt"
	for _, inclusive := range []bool{false, true} {
		got, err := tax.Calculate(dest, items, inclusive, start, r)
		check(t, got, err, tax.Amounts{Net: 100, Tax: 0, Gross: 100})
		if got.Lines[0].Snapshot.RateID != "" || got.Lines[0].Snapshot.BasisPoints != 0 || got.Lines[0].Snapshot.Reason != "exempt" {
			t.Fatalf("exempt rate leaked into snapshot: %+v", got.Lines[0].Snapshot)
		}
	}
}

func TestRejectInputsAndRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*tax.Ruleset, *tax.Destination, *[]tax.LineItem, *time.Time)
		want   error
	}{
		{"negative", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { (*l)[0].UnitAmount = -1 }, tax.ErrInvalidInput},
		{"zero qty", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { (*l)[0].Quantity = 0 }, tax.ErrInvalidInput},
		{"negative qty", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { (*l)[0].Quantity = -1 }, tax.ErrInvalidInput},
		{"empty items", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { *l = nil }, tax.ErrInvalidInput},
		{"missing time", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { *a = time.Time{} }, tax.ErrInvalidInput},
		{"lower country", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { d.Country = "us" }, tax.ErrInvalidInput},
		{"unknown country", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			d.Country = "ZZ"
			d.Subdivision = ""
		}, tax.ErrInvalidInput},
		{"wrong subdivision", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { d.Subdivision = "CA-ON" }, tax.ErrInvalidInput},
		{"subdivision lowercase", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { d.Subdivision = "US-ca" }, tax.ErrInvalidInput},
		{"subdivision overlength", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { d.Subdivision = "US-ABCD" }, tax.ErrInvalidInput},
		{"no zone", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Zones = nil
			r.Rates = nil
		}, tax.ErrZoneNotFound},
		{"no category", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			(*l)[0].Category = "missing"
		}, tax.ErrCategoryNotFound},
		{"no rate", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Rates = nil }, tax.ErrRateNotFound},
		{"ambiguous rate", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			s := r.Rates[0]
			s.ID = "second"
			r.Rates = append(r.Rates, s)
		}, tax.ErrAmbiguousRate},
		{"bad bp negative", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Rates[0].BasisPoints = -1 }, tax.ErrInvalidConfig},
		{"bad bp high", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Rates[0].BasisPoints = 10001
		}, tax.ErrInvalidConfig},
		{"empty window", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Rates[0].ValidUntil = &r.Rates[0].ValidFrom
		}, tax.ErrInvalidConfig},
		{"reversed window", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			v := start.Add(-time.Hour)
			r.Rates[0].ValidUntil = &v
		}, tax.ErrInvalidConfig},
		{"duplicate rate id", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Rates = append(r.Rates, r.Rates[0])
		}, tax.ErrInvalidConfig},
		{"duplicate category", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Categories = append(r.Categories, r.Categories[0])
		}, tax.ErrInvalidConfig},
		{"duplicate zone id", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Zones = append(r.Zones, r.Zones[0])
		}, tax.ErrInvalidConfig},
		{"duplicate mapping", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Zones = append(r.Zones, tax.Zone{ID: "other", Locations: []tax.Destination{{Country: "US"}}})
		}, tax.ErrInvalidConfig},
		{"same zone duplicate mapping", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Zones[0].Locations = append(r.Zones[0].Locations, r.Zones[0].Locations[0])
		}, tax.ErrInvalidConfig},
		{"unknown rate zone", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Rates[0].ZoneID = "other" }, tax.ErrInvalidConfig},
		{"unknown rate category", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Rates[0].Category = "other"
		}, tax.ErrInvalidConfig},
		{"bad zone country", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) {
			r.Zones[0].Locations[0].Country = "UK"
		}, tax.ErrInvalidConfig},
		{"empty zone id", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Zones[0].ID = "" }, tax.ErrInvalidConfig},
		{"empty category code", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Categories[0].Code = "" }, tax.ErrInvalidConfig},
		{"empty rate id", func(r *tax.Ruleset, d *tax.Destination, l *[]tax.LineItem, a *time.Time) { r.Rates[0].ID = "" }, tax.ErrInvalidConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d, l, a := rules(2000), dest, item(100, 1), start
			tc.mutate(&r, &d, &l, &a)
			got, err := tax.Calculate(d, l, false, a, r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if !reflect.DeepEqual(got, tax.Result{}) {
				t.Fatalf("partial result on error: %+v", got)
			}
		})
	}
}

func TestOverflowAndWideIntermediates(t *testing.T) {
	for _, tc := range []struct {
		name            string
		amount, qty, bp int64
		inclusive       bool
		overflow        bool
		want            tax.Amounts
	}{
		{"max zero", math.MaxInt64, 1, 0, false, false, tax.Amounts{Net: math.MaxInt64, Gross: math.MaxInt64}},
		{"max inclusive", math.MaxInt64, 1, 10000, true, false, tax.Amounts{Net: 4611686018427387903, Tax: 4611686018427387904, Gross: math.MaxInt64}},
		{"wide exclusive", math.MaxInt64 / 2, 1, 10000, false, false, tax.Amounts{Net: math.MaxInt64 / 2, Tax: math.MaxInt64 / 2, Gross: math.MaxInt64 - 1}},
		{"gross overflow", math.MaxInt64, 1, 1, false, true, tax.Amounts{}},
		{"quantity overflow", math.MaxInt64, 2, 0, false, true, tax.Amounts{}},
		{"zero huge qty", 0, math.MaxInt64, 10000, false, false, tax.Amounts{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tax.Calculate(dest, item(tc.amount, tc.qty), tc.inclusive, start, rules(tc.bp))
			if tc.overflow {
				if !errors.Is(err, tax.ErrOverflow) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			check(t, got, err, tc.want)
		})
	}
	_, err := tax.Calculate(dest, append(item(math.MaxInt64, 1), item(1, 1)...), true, start, rules(0))
	if !errors.Is(err, tax.ErrOverflow) {
		t.Fatalf("total overflow err=%v", err)
	}
}

func TestRandomOracleAndConservation(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	for i := 0; i < 1500; i++ {
		amount, qty, bp := rng.Int63n(1000000000), int64(rng.Intn(20)+1), int64(rng.Intn(10001))
		inclusive := i%2 == 0
		r := rules(bp)
		l := item(amount, qty)
		if i%7 == 0 {
			l[0].Category = "exempt"
		} else if i%11 == 0 {
			r.Rates[0].BasisPoints = 0
			bp = 0
		}
		got, err := tax.Calculate(dest, l, inclusive, start, r)
		if err != nil {
			t.Fatal(err)
		}
		a := got.Lines[0].Amounts
		base := amount * qty
		den := int64(10000)
		if inclusive {
			den += bp
		}
		fraction := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(base), big.NewInt(bp)), big.NewInt(den))
		fraction.Add(fraction, big.NewRat(1, 2))
		wantTax := new(big.Int).Quo(fraction.Num(), fraction.Denom()).Int64()
		if l[0].Category == "exempt" {
			wantTax = 0
		}
		if a.Tax != wantTax || a.Net+a.Tax != a.Gross || a.Net < 0 || a.Tax < 0 || a.Gross < 0 || a != got.Totals {
			t.Fatalf("i=%d amounts=%+v wantTax=%d", i, a, wantTax)
		}
		if inclusive && a.Gross != base || !inclusive && a.Net != base {
			t.Fatalf("i=%d price changed", i)
		}
	}
}

func TestCountriesAndSubdivisionSyntax(t *testing.T) {
	// Independent edge samples include assigned territories; aliases/nonassigned
	// codes are rejected. Registry membership of subdivision is intentionally absent.
	for _, country := range []string{"AD", "AX", "BQ", "BV", "CW", "HM", "IO", "PS", "SS", "SX", "TW", "UM", "ZW"} {
		r := rules(0)
		r.Zones[0].Locations[0].Country = country
		got, err := tax.Calculate(tax.Destination{Country: country, Subdivision: country + "-ZZZ"}, item(1, 1), false, start, r)
		check(t, got, err, tax.Amounts{Net: 1, Gross: 1})
	}
	for _, country := range []string{"AA", "AN", "EU", "UK", "XK", "ZZ", "us", "USA", ""} {
		_, err := tax.Calculate(tax.Destination{Country: country}, item(1, 1), false, start, rules(0))
		if !errors.Is(err, tax.ErrInvalidInput) {
			t.Fatalf("country %q err=%v", country, err)
		}
	}
}

func TestAssignedCountryRegistryDigest(t *testing.T) {
	// Enumerate public behavior instead of copying the production country table.
	// Digest pins all 249 assigned alpha-2 codes, including territory membership.
	var accepted []string
	for first := 'A'; first <= 'Z'; first++ {
		for second := 'A'; second <= 'Z'; second++ {
			country := string([]rune{first, second})
			_, err := tax.Calculate(tax.Destination{Country: country}, item(0, 1), false, start, rules(0))
			if errors.Is(err, tax.ErrZoneNotFound) || err == nil {
				accepted = append(accepted, country)
			} else if !errors.Is(err, tax.ErrInvalidInput) {
				t.Fatalf("country %s: unexpected err=%v", country, err)
			}
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(accepted, " "))))
	if len(accepted) != 249 || digest != "e1c950d24ceb933ac49eaa44de8d1a7ffb6bf40bfe519a5a99d791cb50332197" {
		t.Fatalf("country registry count=%d digest=%s", len(accepted), digest)
	}
}

func TestRejectUnserializableRateWindows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  time.Time
		until *time.Time
		at    time.Time
	}{
		{"from year 10000", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), nil, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"from year zero", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), nil, start},
		{"from invalid offset", time.Date(2025, 1, 1, 0, 0, 0, 0, time.FixedZone("invalid", 24*60*60)), nil, start},
		{"until year 10000", start, timePointer(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)), start},
		{"until invalid offset", start, timePointer(time.Date(2027, 1, 1, 0, 0, 0, 0, time.FixedZone("invalid", -24*60*60))), start},
		{"UTC from outside window", time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 60*60)), nil, start},
		{"UTC until outside window", start, timePointer(time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("offset", -60*60))), start},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rules(2000)
			r.Rates[0].ValidFrom, r.Rates[0].ValidUntil = tc.from, tc.until
			got, err := tax.Calculate(dest, item(100, 1), false, tc.at, r)
			if !errors.Is(err, tax.ErrInvalidConfig) {
				encoded, marshalErr := json.Marshal(got)
				t.Fatalf("err=%v want invalid config; snapshot JSON=%s error=%v", err, encoded, marshalErr)
			}
		})
	}
	// The supported ends remain valid and serialize, with no artificial expiry.
	for _, from := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)} {
		r := rules(2000)
		r.Rates[0].ValidFrom, r.Rates[0].ValidUntil = from, nil
		got, err := tax.Calculate(dest, item(100, 1), false, from.Add(time.Second), r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatal(err)
		}
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestSnapshotJSONPreservesOffsetInstants(t *testing.T) {
	r := rules(2000)
	r.Rates[0].ValidFrom = start.In(time.FixedZone("seconds-offset", 3601))
	until := end.In(time.FixedZone("seconds-offset", -3601))
	r.Rates[0].ValidUntil = &until
	got, err := tax.Calculate(dest, item(100, 1), false, start, r)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded tax.Result
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	s := decoded.Lines[0].Snapshot
	if !s.ValidFrom.Equal(start) || !s.ValidUntil.Equal(end) {
		t.Fatalf("JSON changed rate instants: from=%v until=%v", s.ValidFrom, s.ValidUntil)
	}
}
