// Package tax owns pure destination/rate selection and line tax calculation.
// Dependencies: internal/money for checked arithmetic; stdlib for rule selection.
// Owns no tables, IO, clocks, or mutable runtime state.
// Callers: future Orders/Billing supply one currency, explicit time and rules.
// Invariants: nonnegative int64 minor units, line half-up, net+tax=gross;
// returned snapshots do not alias rules, and missing rules never imply zero tax.
package tax

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

var (
	ErrInvalidInput     = errors.New("tax: invalid input")
	ErrInvalidConfig    = errors.New("tax: invalid ruleset")
	ErrZoneNotFound     = errors.New("tax: destination has no zone")
	ErrCategoryNotFound = errors.New("tax: category not found")
	ErrRateNotFound     = errors.New("tax: active rate not found")
	ErrAmbiguousRate    = errors.New("tax: multiple active rates")
	ErrOverflow         = money.ErrOverflow
)

type Destination struct {
	Country     string `json:"country"`
	Subdivision string `json:"subdivision"`
}
type Zone struct {
	ID        string        `json:"id"`
	Locations []Destination `json:"locations"`
}
type Category struct {
	Code     string `json:"code"`
	IsExempt bool   `json:"is_exempt"`
}
type Rate struct {
	ID          string     `json:"id"`
	ZoneID      string     `json:"zone_id"`
	Category    string     `json:"category"`
	BasisPoints int64      `json:"basis_points"`
	ValidFrom   time.Time  `json:"valid_from"`
	ValidUntil  *time.Time `json:"valid_until"`
}
type Ruleset struct {
	Zones      []Zone     `json:"zones"`
	Categories []Category `json:"categories"`
	Rates      []Rate     `json:"rates"`
}

// UnitAmount is the caller's net price, or gross price when includeTax is true.
// Quantity multiplication precedes rounding; each resulting line rounds once.
// Currency uses the Money contract: three uppercase ASCII letters.
type LineItem struct {
	Currency   string `json:"currency"`
	ID         string `json:"id"`
	Category   string `json:"category"`
	UnitAmount int64  `json:"unit_amount"`
	Quantity   int64  `json:"quantity"`
}

// Snapshot is the serializable decision record. Exempt categories have no rate
// or validity window; a nil ValidUntil for a selected rate means unbounded.
type Snapshot struct {
	Currency    string     `json:"currency"`
	ZoneID      string     `json:"zone_id"`
	RateID      string     `json:"rate_id"`
	Category    string     `json:"category"`
	BasisPoints int64      `json:"basis_points"`
	ValidFrom   *time.Time `json:"valid_from"`
	ValidUntil  *time.Time `json:"valid_until"`
	IncludeTax  bool       `json:"include_tax"`
	Rounding    string     `json:"rounding"`
	Reason      string     `json:"reason"`
}
type Amounts struct {
	Net   int64 `json:"net"`
	Tax   int64 `json:"tax"`
	Gross int64 `json:"gross"`
}
type LineResult struct {
	ID       string   `json:"id"`
	Amounts  Amounts  `json:"amounts"`
	Snapshot Snapshot `json:"snapshot"`
}
type Result struct {
	Currency string       `json:"currency"`
	Lines    []LineResult `json:"lines"`
	Totals   Amounts      `json:"totals"`
}

// Calculate validates the complete supplied ruleset and selects an exact
// subdivision before its country wildcard. It never falls back between zones
// for a rate. Active [ValidFrom, ValidUntil) rates must be unique for each line.
// Currency is required and identical across lines; no conversion is implicit.
// Tenant isolation and loading consistent rules are caller concerns.
func Calculate(destination Destination, items []LineItem, includeTax bool, at time.Time, rules Ruleset) (Result, error) {
	if !validDestination(destination) || len(items) == 0 || at.IsZero() {
		return Result{}, ErrInvalidInput
	}
	for i, item := range items {
		if err := (money.Money{Amount: item.UnitAmount, Currency: item.Currency}).Validate(); err != nil {
			return Result{}, fmt.Errorf("line %d: %w: %w", i, ErrInvalidInput, err)
		}
		if item.Currency != items[0].Currency {
			return Result{}, fmt.Errorf("line %d: %w: %w", i, ErrInvalidInput, money.ErrCurrencyMismatch)
		}
		if item.UnitAmount < 0 || item.Quantity <= 0 || !validCategoryCode(item.Category) {
			return Result{}, fmt.Errorf("line %d: %w", i, ErrInvalidInput)
		}
	}
	if err := validateRules(rules); err != nil {
		return Result{}, err
	}
	zoneID := ""
	for _, zone := range rules.Zones {
		for _, location := range zone.Locations {
			if location.Country == destination.Country && location.Subdivision == "" {
				zoneID = zone.ID
			}
		}
	}
	if destination.Subdivision != "" {
		for _, zone := range rules.Zones {
			for _, location := range zone.Locations {
				if location == destination {
					zoneID = zone.ID
				}
			}
		}
	}
	if zoneID == "" {
		return Result{}, ErrZoneNotFound
	}
	result := Result{Currency: items[0].Currency, Lines: make([]LineResult, 0, len(items))}
	for i, item := range items {
		var category Category
		found := false
		for _, c := range rules.Categories {
			if c.Code == item.Category {
				category, found = c, true
				break
			}
		}
		if !found {
			return Result{}, fmt.Errorf("line %d: %w", i, ErrCategoryNotFound)
		}
		snapshot := Snapshot{Currency: item.Currency, ZoneID: zoneID, Category: category.Code, IncludeTax: includeTax, Rounding: "half_up", Reason: "rate"}
		if category.IsExempt {
			snapshot.Reason = "exempt"
		} else {
			selected := false
			for _, rate := range rules.Rates {
				if rate.ZoneID == zoneID && rate.Category == category.Code && !at.Before(rate.ValidFrom) && (rate.ValidUntil == nil || at.Before(*rate.ValidUntil)) {
					if selected {
						return Result{}, fmt.Errorf("line %d: %w", i, ErrAmbiguousRate)
					}
					selected = true
					snapshot.RateID, snapshot.BasisPoints = rate.ID, rate.BasisPoints
					// Canonical UTC preserves instants through JSON, whose offset
					// format cannot represent seconds in a FixedZone offset.
					from := rate.ValidFrom.UTC()
					snapshot.ValidFrom = &from
					if rate.ValidUntil != nil {
						until := rate.ValidUntil.UTC()
						snapshot.ValidUntil = &until
					}
				}
			}
			if !selected {
				return Result{}, fmt.Errorf("line %d: %w", i, ErrRateNotFound)
			}
		}
		base, err := (money.Money{Amount: item.UnitAmount, Currency: item.Currency}).Mul(item.Quantity)
		if err != nil {
			return Result{}, fmt.Errorf("line %d: %w", i, err)
		}
		denominator := int64(10000)
		if includeTax {
			denominator += snapshot.BasisPoints
		}
		taxAmount, err := base.Scale(snapshot.BasisPoints, denominator)
		if err != nil {
			return Result{}, fmt.Errorf("line %d: %w", i, err)
		}
		net, gross := base, base
		if includeTax {
			net, err = base.Sub(taxAmount)
		} else {
			gross, err = base.Add(taxAmount)
		}
		if err != nil {
			return Result{}, fmt.Errorf("line %d: %w", i, err)
		}
		amounts := Amounts{Net: net.Amount, Tax: taxAmount.Amount, Gross: gross.Amount}
		for _, pair := range []struct {
			total *int64
			line  money.Money
		}{
			{&result.Totals.Net, net}, {&result.Totals.Tax, taxAmount}, {&result.Totals.Gross, gross},
		} {
			total, err := (money.Money{Amount: *pair.total, Currency: result.Currency}).Add(pair.line)
			if err != nil {
				return Result{}, err
			}
			*pair.total = total.Amount
		}
		result.Lines = append(result.Lines, LineResult{ID: item.ID, Amounts: amounts, Snapshot: snapshot})
	}
	return result, nil
}

// ponytail: linear scans keep the supplied small ruleset explicit; add indexes
// only after measuring large tenant rulesets, without changing this contract.
func validateRules(rules Ruleset) error {
	zones, categories, rates := map[string]bool{}, map[string]bool{}, map[string]bool{}
	locations := map[Destination]bool{}
	for _, zone := range rules.Zones {
		if strings.TrimSpace(zone.ID) == "" || zones[zone.ID] || len(zone.Locations) == 0 {
			return fmt.Errorf("zone %q: %w", zone.ID, ErrInvalidConfig)
		}
		zones[zone.ID] = true
		for _, location := range zone.Locations {
			if !validDestination(location) || locations[location] {
				return fmt.Errorf("location %+v: %w", location, ErrInvalidConfig)
			}
			locations[location] = true
		}
	}
	for _, category := range rules.Categories {
		if !validCategoryCode(category.Code) || categories[category.Code] {
			return fmt.Errorf("category %q: %w", category.Code, ErrInvalidConfig)
		}
		categories[category.Code] = true
	}
	for _, rate := range rules.Rates {
		if strings.TrimSpace(rate.ID) == "" || rates[rate.ID] || !zones[rate.ZoneID] || !categories[rate.Category] || rate.BasisPoints < 0 || rate.BasisPoints > 10000 || !validRateTime(rate.ValidFrom) || (rate.ValidUntil != nil && (!validRateTime(*rate.ValidUntil) || !rate.ValidFrom.Before(*rate.ValidUntil))) {
			return fmt.Errorf("rate %q: %w", rate.ID, ErrInvalidConfig)
		}
		rates[rate.ID] = true
	}
	return nil
}

// Snapshots must remain JSON serializable. UTC bounds also match the SQL
// timestamptz contract, even if callers construct an offset near a year edge.
func validRateTime(value time.Time) bool {
	year := value.UTC().Year()
	if year < 1 || year > 9999 {
		return false
	}
	_, err := value.MarshalJSON()
	return err == nil
}

func validCategoryCode(code string) bool {
	if len(code) < 1 || len(code) > 64 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Country membership is ISO 3166-1 alpha-2; subdivisions are syntax/prefix
// only (one to three uppercase ASCII letters/digits), not a registry lookup.
func validDestination(destination Destination) bool {
	i := sort.SearchStrings(countryCodes, destination.Country)
	if i == len(countryCodes) || countryCodes[i] != destination.Country {
		return false
	}
	subdivision := destination.Subdivision
	if subdivision == "" {
		return true
	}
	if len(subdivision) < 4 || len(subdivision) > 6 || !strings.HasPrefix(subdivision, destination.Country+"-") {
		return false
	}
	for _, c := range subdivision[3:] {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Sorted assigned countries, shared contract with shipping and SQL constraints.
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
