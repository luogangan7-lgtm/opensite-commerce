// Package shipping computes deterministic, tenant-scoped shipping quotes.
// It depends only on the standard library; callers supply rules already scoped
// to a tenant. Quote never mutates inputs or performs I/O.
package shipping

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type Money struct {
	Amount   int64
	Currency string
}

type Destination struct {
	Country     string
	Subdivision string
}

type Zone struct {
	ID           string
	Destinations []Destination
}

type Rate struct {
	ID, ProfileID, ZoneID, Method string
	Currency                      string
	Amount                        int64
	// MinWeightG is zero for fixed/free rates; a future SQL adapter maps their
	// NULL min_weight_g to zero. A by_weight minimum of zero is an actual bound.
	MinWeightG     int64
	MaxWeightG     *int64
	FreeOverAmount *int64
}

type Request struct {
	ProfileID   string
	Destination Destination
	WeightG     int64
	Subtotal    Money
}

type Result struct {
	Charge                 Money
	ZoneID, RateID, Method string
}

var (
	ErrInvalid          = errors.New("shipping: invalid input")
	ErrNoZone           = errors.New("shipping: no matching zone")
	ErrNoRate           = errors.New("shipping: no matching rate")
	ErrAmbiguousZone    = errors.New("shipping: ambiguous zone")
	ErrCurrencyMismatch = errors.New("shipping: currency mismatch")
)

// Quote validates the complete supplied configuration before selecting a zone.
// Subdivision rules beat country rules; ties across zones are refused. Free
// shipping is evaluated first, then weight tiers, then fixed only without tiers.
func Quote(zones []Zone, rates []Rate, request Request) (Result, error) {
	if !validID(request.ProfileID) || !validDestination(request.Destination) ||
		request.WeightG < 0 || request.Subtotal.Amount < 0 || !validCurrency(request.Subtotal.Currency) {
		return Result{}, fmt.Errorf("%w: request", ErrInvalid)
	}
	if err := validate(zones, rates); err != nil {
		return Result{}, err
	}
	zoneID, err := matchingZone(zones, request.Destination)
	if err != nil {
		return Result{}, err
	}
	var fixed, free, weight *Rate
	hasWeight := false
	for i := range rates {
		r := &rates[i]
		if r.ProfileID != request.ProfileID || r.ZoneID != zoneID {
			continue
		}
		if r.Currency != request.Subtotal.Currency {
			return Result{}, ErrCurrencyMismatch
		}
		switch r.Method {
		case "fixed":
			fixed = r
		case "free_over_amount":
			free = r
		case "by_weight":
			hasWeight = true
			if request.WeightG >= r.MinWeightG && (r.MaxWeightG == nil || request.WeightG < *r.MaxWeightG) {
				weight = r
			}
		}
	}
	var selected *Rate
	switch {
	case free != nil && request.Subtotal.Amount >= *free.FreeOverAmount:
		selected = free
	case hasWeight:
		selected = weight
	default:
		selected = fixed
	}
	if selected == nil {
		return Result{}, ErrNoRate
	}
	return Result{Charge: Money{Amount: selected.Amount, Currency: selected.Currency},
		ZoneID: zoneID, RateID: selected.ID, Method: selected.Method}, nil
}

func matchingZone(zones []Zone, destination Destination) (string, error) {
	best, matches, selected := -1, 0, ""
	for _, z := range zones {
		specificity := -1
		for _, d := range z.Destinations {
			if d.Country != destination.Country {
				continue
			}
			if d.Subdivision == "" && specificity < 0 {
				specificity = 0
			} else if d.Subdivision != "" && d.Subdivision == destination.Subdivision {
				specificity = 1
			}
		}
		if specificity < 0 {
			continue
		}
		if specificity > best {
			best, matches, selected = specificity, 1, z.ID
		} else if specificity == best {
			matches++
		}
	}
	if matches == 0 {
		return "", ErrNoZone
	}
	if matches > 1 {
		return "", ErrAmbiguousZone
	}
	return selected, nil
}

// Validation mirrors storage constraints so invalid rules cannot silently
// produce a plausible quote, even if the invalid row would not be selected.
func validate(zones []Zone, rates []Rate) error {
	zoneIDs := make(map[string]bool, len(zones))
	for _, z := range zones {
		if !validID(z.ID) || zoneIDs[z.ID] {
			return fmt.Errorf("%w: zone identity", ErrInvalid)
		}
		zoneIDs[z.ID] = true
		seen := make(map[Destination]bool, len(z.Destinations))
		for _, d := range z.Destinations {
			if !validDestination(d) || seen[d] {
				return fmt.Errorf("%w: zone destination", ErrInvalid)
			}
			seen[d] = true
		}
	}
	type key struct{ profile, zone string }
	type group struct {
		fixed, free bool
		tiers       []Rate
	}
	groups := make(map[key]*group)
	profileCurrencies := make(map[string]string)
	rateIDs := make(map[string]bool, len(rates))
	for _, r := range rates {
		if !validID(r.ID) || rateIDs[r.ID] || !validID(r.ProfileID) || !zoneIDs[r.ZoneID] ||
			!validCurrency(r.Currency) || r.Amount < 0 || r.MinWeightG < 0 {
			return fmt.Errorf("%w: rate identity, currency, or amount", ErrInvalid)
		}
		rateIDs[r.ID] = true
		if currency, exists := profileCurrencies[r.ProfileID]; exists && currency != r.Currency {
			return fmt.Errorf("%w: profile rate currencies", ErrCurrencyMismatch)
		}
		profileCurrencies[r.ProfileID] = r.Currency
		k := key{r.ProfileID, r.ZoneID}
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
		}
		switch r.Method {
		case "fixed":
			if g.fixed || r.MinWeightG != 0 || r.MaxWeightG != nil || r.FreeOverAmount != nil {
				return fmt.Errorf("%w: fixed rate shape or multiplicity", ErrInvalid)
			}
			g.fixed = true
		case "free_over_amount":
			if g.free || r.Amount != 0 || r.MinWeightG != 0 || r.MaxWeightG != nil || r.FreeOverAmount == nil || *r.FreeOverAmount < 0 {
				return fmt.Errorf("%w: free rate shape or multiplicity", ErrInvalid)
			}
			g.free = true
		case "by_weight":
			if r.FreeOverAmount != nil || (r.MaxWeightG != nil && *r.MaxWeightG <= r.MinWeightG) {
				return fmt.Errorf("%w: weight rate shape", ErrInvalid)
			}
			for _, previous := range g.tiers {
				if (previous.MaxWeightG == nil || r.MinWeightG < *previous.MaxWeightG) &&
					(r.MaxWeightG == nil || previous.MinWeightG < *r.MaxWeightG) {
					return fmt.Errorf("%w: overlapping weight rates", ErrInvalid)
				}
			}
			g.tiers = append(g.tiers, r)
		default:
			return fmt.Errorf("%w: rate method", ErrInvalid)
		}
	}
	return nil
}

func validID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func validCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for i := range value {
		if value[i] < 'A' || value[i] > 'Z' {
			return false
		}
	}
	return true
}

func validDestination(d Destination) bool {
	if !validCountry(d.Country) {
		return false
	}
	if d.Subdivision == "" {
		return true
	}
	if !strings.HasPrefix(d.Subdivision, d.Country+"-") || len(d.Subdivision) < 4 || len(d.Subdivision) > 6 {
		return false
	}
	for i := 3; i < len(d.Subdivision); i++ {
		c := d.Subdivision[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// ISO 3166-1 alpha-2 assigned codes, sorted for deterministic binary search.
// Subdivisions are validated for ISO 3166-2-style syntax, not registry membership.
var countryCodes = strings.Fields("AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ " +
	"BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
	"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ " +
	"DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
	"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY " +
	"HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
	"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY " +
	"MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ " +
	"NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY " +
	"QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ " +
	"TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ " +
	"VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW")

func validCountry(value string) bool {
	i := sort.SearchStrings(countryCodes, value)
	return i < len(countryCodes) && countryCodes[i] == value
}
