// Shipping semantic gates cover rule shape, refusal, boundaries, and purity.
package shipping

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

func number(n int64) *int64 { return &n }

func standardZone() []Zone {
	return []Zone{{ID: "country", Destinations: []Destination{{Country: "US"}}}}
}

func standardRequest() Request {
	return Request{ProfileID: "standard", Destination: Destination{Country: "US", Subdivision: "US-CA"}, Subtotal: Money{Currency: "USD"}}
}

func rate(id, method string, amount int64) Rate {
	return Rate{ID: id, ProfileID: "standard", ZoneID: "country", Method: method, Currency: "USD", Amount: amount}
}

func TestQuoteBoundaries(t *testing.T) {
	tier1 := rate("light", "by_weight", 500)
	tier1.MaxWeightG = number(1000)
	tier2 := rate("heavy", "by_weight", 800)
	tier2.MinWeightG, tier2.MaxWeightG = 1000, number(2000)
	free := rate("free", "free_over_amount", 0)
	free.FreeOverAmount = number(10000)
	fixed := rate("fixed", "fixed", 1500)
	unlimited := rate("unlimited", "by_weight", 900)
	unlimited.MinWeightG = 2000
	tests := []struct {
		name             string
		rates            []Rate
		weight, subtotal int64
		id               string
		amount           int64
		err              error
	}{
		{"zero weight", []Rate{tier1, tier2}, 0, 0, "light", 500, nil},
		{"below boundary", []Rate{tier1, tier2}, 999, 0, "light", 500, nil},
		{"exact min and previous max", []Rate{tier1, tier2}, 1000, 0, "heavy", 800, nil},
		{"exclusive max no fixed fallback", []Rate{tier1, tier2, fixed}, 2000, 0, "", 0, ErrNoRate},
		{"above max", []Rate{tier1, tier2}, 2001, 0, "", 0, ErrNoRate},
		{"gap no fixed fallback", []Rate{tier1, unlimited, fixed}, 1500, 0, "", 0, ErrNoRate},
		{"unlimited exact min", []Rate{tier1, unlimited}, 2000, 0, "unlimited", 900, nil},
		{"unlimited int64 max", []Rate{unlimited}, 1<<63 - 1, 0, "unlimited", 900, nil},
		{"fixed", []Rate{fixed}, 20000, 0, "fixed", 1500, nil},
		{"free inclusive", []Rate{tier1, fixed, free}, 2000, 10000, "free", 0, nil},
		{"free above", []Rate{free}, 0, 10001, "free", 0, nil},
		{"below free with fixed", []Rate{free, fixed}, 0, 9999, "fixed", 1500, nil},
		{"below free alone", []Rate{free}, 0, 9999, "", 0, ErrNoRate},
		{"no rates", nil, 0, 0, "", 0, ErrNoRate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := standardRequest()
			r.WeightG, r.Subtotal.Amount = tt.weight, tt.subtotal
			got, err := Quote(standardZone(), tt.rates, r)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Quote error = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				if got != (Result{}) {
					t.Fatalf("error returned a usable quote: %+v", got)
				}
				return
			}
			if got.Charge != (Money{Amount: tt.amount, Currency: "USD"}) || got.RateID != tt.id || got.ZoneID != "country" {
				t.Fatalf("Quote = %+v, want %s amount %d USD", got, tt.id, tt.amount)
			}
			for _, selected := range tt.rates {
				if selected.ID == got.RateID && got.Method != selected.Method {
					t.Fatalf("method = %s, want %s", got.Method, selected.Method)
				}
			}
		})
	}
}

func TestQuoteZoneSelection(t *testing.T) {
	specific := Zone{ID: "california", Destinations: []Destination{{Country: "US", Subdivision: "US-CA"}}}
	specificRate := rate("ca", "fixed", 300)
	specificRate.ZoneID = specific.ID
	fixed := rate("fixed", "fixed", 500)
	tests := []struct {
		name        string
		zones       []Zone
		destination Destination
		zone        string
		err         error
	}{
		{"subdivision precedence", append(standardZone(), specific), Destination{"US", "US-CA"}, "california", nil},
		{"missing subdivision country", append(standardZone(), specific), Destination{"US", ""}, "country", nil},
		{"missing subdivision specific only", []Zone{specific}, Destination{"US", ""}, "", ErrNoZone},
		{"other subdivision country", append(standardZone(), specific), Destination{"US", "US-NY"}, "country", nil},
		{"no country", standardZone(), Destination{"CA", "CA-ON"}, "", ErrNoZone},
		{"ambiguous country", append(standardZone(), Zone{ID: "second", Destinations: []Destination{{Country: "US"}}}), Destination{"US", ""}, "", ErrAmbiguousZone},
		{"ambiguous subdivision", []Zone{specific, {ID: "second", Destinations: specific.Destinations}}, Destination{"US", "US-CA"}, "", ErrAmbiguousZone},
		{"specific beats ambiguous countries", append(standardZone(), Zone{ID: "second", Destinations: []Destination{{Country: "US"}}}, specific), Destination{"US", "US-CA"}, "california", nil},
		{"same zone multiple matches", []Zone{{ID: "country", Destinations: []Destination{{Country: "US"}, {Country: "US", Subdivision: "US-CA"}}}}, Destination{"US", "US-CA"}, "country", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := standardRequest()
			r.Destination = tt.destination
			// Only rates for supplied zones: callers pass tenant-scoped configuration.
			var rates []Rate
			for _, z := range tt.zones {
				if z.ID == "country" {
					rates = append(rates, fixed)
				}
				if z.ID == "california" {
					rates = append(rates, specificRate)
				}
			}
			got, err := Quote(tt.zones, rates, r)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if tt.err == nil && got.ZoneID != tt.zone {
				t.Fatalf("zone = %s, want %s", got.ZoneID, tt.zone)
			}
		})
	}
}

func TestQuoteRefusesInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		change func(*[]Zone, *[]Rate, *Request)
		err    error
	}{
		{"negative weight", func(_ *[]Zone, _ *[]Rate, q *Request) { q.WeightG = -1 }, ErrInvalid},
		{"negative subtotal", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Subtotal.Amount = -1 }, ErrInvalid},
		{"blank profile", func(_ *[]Zone, _ *[]Rate, q *Request) { q.ProfileID = "" }, ErrInvalid},
		{"unknown country", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Country = "ZZ" }, ErrInvalid},
		{"lowercase country", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Country = "us" }, ErrInvalid},
		{"subdivision wrong country", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Subdivision = "CA-ON" }, ErrInvalid},
		{"subdivision empty suffix", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Subdivision = "US-" }, ErrInvalid},
		{"subdivision long suffix", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Subdivision = "US-ABCD" }, ErrInvalid},
		{"subdivision punctuation", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Subdivision = "US-C!" }, ErrInvalid},
		{"subdivision lowercase", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Destination.Subdivision = "US-ca" }, ErrInvalid},
		{"currency lowercase", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Subtotal.Currency = "usd" }, ErrInvalid},
		{"currency unicode", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Subtotal.Currency = "ＵＳＤ" }, ErrInvalid},
		{"currency length", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Subtotal.Currency = "US" }, ErrInvalid},
		{"currency mismatch", func(_ *[]Zone, _ *[]Rate, q *Request) { q.Subtotal.Currency = "EUR" }, ErrCurrencyMismatch},
		{"negative rate", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].Amount = -1 }, ErrInvalid},
		{"bad rate currency", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].Currency = "UsD" }, ErrInvalid},
		{"blank rate id", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].ID = " " }, ErrInvalid},
		{"blank rate profile", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].ProfileID = "" }, ErrInvalid},
		{"unknown rate zone", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].ZoneID = "missing" }, ErrInvalid},
		{"duplicate rate id", func(_ *[]Zone, r *[]Rate, _ *Request) { *r = append(*r, (*r)[0]) }, ErrInvalid},
		{"duplicate fixed", func(_ *[]Zone, r *[]Rate, _ *Request) { *r = append(*r, rate("other", "fixed", 900)) }, ErrInvalid},
		{"mixed currency", func(_ *[]Zone, r *[]Rate, _ *Request) {
			f := rate("free", "free_over_amount", 0)
			f.FreeOverAmount = number(0)
			f.Currency = "EUR"
			*r = append(*r, f)
		}, ErrCurrencyMismatch},
		{"profile currency across zones", func(z *[]Zone, r *[]Rate, _ *Request) {
			*z = append(*z, Zone{ID: "canada", Destinations: []Destination{{Country: "CA"}}})
			other := rate("canada", "fixed", 900)
			other.ZoneID, other.Currency = "canada", "CAD"
			*r = append(*r, other)
		}, ErrCurrencyMismatch},
		{"unknown method", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].Method = "carrier" }, ErrInvalid},
		{"fixed weight min", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].MinWeightG = 1 }, ErrInvalid},
		{"fixed weight max", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].MaxWeightG = number(10) }, ErrInvalid},
		{"fixed threshold", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].FreeOverAmount = number(10) }, ErrInvalid},
		{"free missing threshold", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0] = rate("free", "free_over_amount", 0) }, ErrInvalid},
		{"free negative threshold", func(_ *[]Zone, r *[]Rate, _ *Request) {
			(*r)[0] = rate("free", "free_over_amount", 0)
			(*r)[0].FreeOverAmount = number(-1)
		}, ErrInvalid},
		{"free nonzero charge", func(_ *[]Zone, r *[]Rate, _ *Request) {
			(*r)[0] = rate("free", "free_over_amount", 1)
			(*r)[0].FreeOverAmount = number(10)
		}, ErrInvalid},
		{"free weight bounds", func(_ *[]Zone, r *[]Rate, _ *Request) {
			(*r)[0] = rate("free", "free_over_amount", 0)
			(*r)[0].FreeOverAmount = number(10)
			(*r)[0].MaxWeightG = number(10)
		}, ErrInvalid},
		{"duplicate free", func(_ *[]Zone, r *[]Rate, _ *Request) {
			f := rate("free", "free_over_amount", 0)
			f.FreeOverAmount = number(10)
			g := f
			g.ID = "other"
			*r = []Rate{f, g}
		}, ErrInvalid},
		{"tier negative min", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].Method = "by_weight"; (*r)[0].MinWeightG = -1 }, ErrInvalid},
		{"tier empty range", func(_ *[]Zone, r *[]Rate, _ *Request) { (*r)[0].Method = "by_weight"; (*r)[0].MaxWeightG = number(0) }, ErrInvalid},
		{"tier reversed range", func(_ *[]Zone, r *[]Rate, _ *Request) {
			(*r)[0].Method = "by_weight"
			(*r)[0].MinWeightG = 20
			(*r)[0].MaxWeightG = number(10)
		}, ErrInvalid},
		{"tier threshold", func(_ *[]Zone, r *[]Rate, _ *Request) {
			(*r)[0].Method = "by_weight"
			(*r)[0].FreeOverAmount = number(10)
		}, ErrInvalid},
		{"overlap", func(_ *[]Zone, r *[]Rate, _ *Request) {
			a := rate("a", "by_weight", 1)
			a.MaxWeightG = number(10)
			b := rate("b", "by_weight", 2)
			b.MinWeightG = 9
			*r = []Rate{a, b}
		}, ErrInvalid},
		{"unlimited overlap", func(_ *[]Zone, r *[]Rate, _ *Request) {
			a := rate("a", "by_weight", 1)
			b := rate("b", "by_weight", 2)
			b.MinWeightG = 10
			*r = []Rate{b, a}
		}, ErrInvalid},
		{"blank zone id", func(z *[]Zone, _ *[]Rate, _ *Request) { (*z)[0].ID = "" }, ErrInvalid},
		{"duplicate zone id", func(z *[]Zone, _ *[]Rate, _ *Request) { *z = append(*z, (*z)[0]) }, ErrInvalid},
		{"unknown zone country", func(z *[]Zone, _ *[]Rate, _ *Request) { (*z)[0].Destinations[0].Country = "XX" }, ErrInvalid},
		{"bad zone subdivision", func(z *[]Zone, _ *[]Rate, _ *Request) { (*z)[0].Destinations[0].Subdivision = "CA-ON" }, ErrInvalid},
		{"duplicate zone destination", func(z *[]Zone, _ *[]Rate, _ *Request) {
			(*z)[0].Destinations = append((*z)[0].Destinations, (*z)[0].Destinations[0])
		}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zones, rates, request := standardZone(), []Rate{rate("fixed", "fixed", 500)}, standardRequest()
			tt.change(&zones, &rates, &request)
			got, err := Quote(zones, rates, request)
			if !errors.Is(err, tt.err) || got != (Result{}) {
				t.Fatalf("Quote = %+v, %v; want zero, %v", got, err, tt.err)
			}
		})
	}
}

func TestQuoteProfileScopeAndPurity(t *testing.T) {
	zones := append(standardZone(), Zone{ID: "canada", Destinations: []Destination{{Country: "CA"}}})
	light := rate("light", "by_weight", 123)
	light.MaxWeightG = number(1000)
	heavy := rate("heavy", "by_weight", 456)
	heavy.MinWeightG = 1000
	otherProfile := rate("other-profile", "fixed", 987)
	otherProfile.ProfileID, otherProfile.Currency = "other", "EUR"
	otherZone := rate("other-zone", "fixed", 999)
	otherZone.ZoneID = "canada"
	rates := []Rate{heavy, otherProfile, light, otherZone}
	request := standardRequest()
	// Deep snapshots ensure input slices, destinations, and bound pointers survive.
	wantZones := []Zone{{ID: "country", Destinations: []Destination{{Country: "US"}}}, {ID: "canada", Destinations: []Destination{{Country: "CA"}}}}
	wantRates := append([]Rate(nil), rates...)
	wantRates[2].MaxWeightG = number(1000)
	got, err := Quote(zones, rates, request)
	if err != nil || got.RateID != "light" || got.Charge.Amount != 123 {
		t.Fatalf("Quote = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(zones, wantZones) || !reflect.DeepEqual(rates, wantRates) || request != standardRequest() {
		t.Fatal("Quote mutated its input")
	}
	reversedZones := []Zone{zones[1], zones[0]}
	reversedRates := []Rate{rates[3], rates[2], rates[1], rates[0]}
	again, err := Quote(reversedZones, reversedRates, request)
	if err != nil || again != got {
		t.Fatalf("order changed Quote: %+v, %v; want %+v", again, err, got)
	}
	request.ProfileID = "missing"
	if _, err := Quote(zones, rates, request); !errors.Is(err, ErrNoRate) {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestCountryRegistryAndDestinationSyntax(t *testing.T) {
	if len(countryCodes) != 249 || !sort.StringsAreSorted(countryCodes) {
		t.Fatalf("registry has %d entries; want 249 sorted assigned countries", len(countryCodes))
	}
	for i, country := range countryCodes {
		if len(country) != 2 || (i > 0 && countryCodes[i-1] == country) || !validCountry(country) {
			t.Fatalf("invalid registry entry %q", country)
		}
		zones := []Zone{{ID: "country", Destinations: []Destination{{Country: country}}}}
		r := standardRequest()
		r.Destination = Destination{Country: country}
		if _, err := Quote(zones, []Rate{rate("fixed", "fixed", 0)}, r); err != nil {
			t.Fatalf("registered country %s rejected: %v", country, err)
		}
	}
	for _, country := range []string{"ZZ", "XX", "UK", "XK", "US ", "", "USA", "us", "1A"} {
		if validCountry(country) {
			t.Fatalf("unregistered country accepted: %q", country)
		}
	}
	for _, destination := range []Destination{{"US", "US-A"}, {"US", "US-ABC"}, {"GB", "GB-123"}, {"TW", "TW-1"}} {
		if !validDestination(destination) {
			t.Fatalf("valid ISO-style destination refused: %+v", destination)
		}
	}
}

func TestZeroThresholdAndUnselectedInvalidRules(t *testing.T) {
	free := rate("free", "free_over_amount", 0)
	free.FreeOverAmount = number(0)
	got, err := Quote(standardZone(), []Rate{free}, standardRequest())
	if err != nil || got.RateID != "free" || got.Charge.Amount != 0 {
		t.Fatalf("zero threshold: %+v, %v", got, err)
	}
	invalid := rate("other", "fixed", -1)
	invalid.ProfileID = "other"
	if got, err := Quote(standardZone(), []Rate{free, invalid}, standardRequest()); !errors.Is(err, ErrInvalid) || got != (Result{}) {
		t.Fatalf("unselected invalid rules must be refused: %+v, %v", got, err)
	}
}

func TestSharedMoneyContract(t *testing.T) {
	request := standardRequest()
	request.Subtotal = Money{Amount: math.MaxInt64, Currency: "USD"}
	if _, ok := any(request.Subtotal).(money.Money); !ok {
		t.Fatal("Request.Subtotal must be the shared money.Money type")
	}
	got, err := Quote(standardZone(), []Rate{rate("fixed", "fixed", math.MaxInt64)}, request)
	if err != nil {
		t.Fatalf("MaxInt64 quote: %v", err)
	}
	charge, ok := any(got.Charge).(money.Money)
	if !ok {
		t.Fatal("Result.Charge must be the shared money.Money type")
	}
	if charge.Amount != math.MaxInt64 || charge.Currency != "USD" || charge.Validate() != nil {
		t.Fatalf("charge = %+v", charge)
	}
	// The returned shared type keeps checked arithmetic at the consuming boundary.
	// Quote itself never adds the subtotal and fee.
	if _, err := charge.Add(money.Money{Amount: 1, Currency: "USD"}); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("shared Add overflow = %v", err)
	}
	if _, err := charge.Add(money.Money{Currency: "EUR"}); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("shared Add currency mismatch = %v", err)
	}
	free := rate("free", "free_over_amount", 0)
	free.FreeOverAmount = number(math.MaxInt64)
	got, err = Quote(standardZone(), []Rate{free}, request)
	if err != nil || got.RateID != "free" || got.Charge.Amount != 0 {
		t.Fatalf("MaxInt64 inclusive free threshold = %+v, %v", got, err)
	}
	request.Subtotal.Amount--
	if _, err := Quote(standardZone(), []Rate{free}, request); !errors.Is(err, ErrNoRate) {
		t.Fatalf("below MaxInt64 threshold = %v", err)
	}
}
