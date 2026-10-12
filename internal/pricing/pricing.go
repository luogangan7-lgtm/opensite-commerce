// Package pricing selects exact SKU tiers from caller-authorized tenant lists.
// It owns pricing rules, depends only on money/stdlib, and performs no I/O/auth.
// sqlc consumers supply canonical UUID strings; SQL and Go use the same tie order.
package pricing

import (
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"time"
)

var ErrInvalid = errors.New("invalid pricing input")
var ErrNoPrice = errors.New("no applicable price")

type Entry struct {
	SKUID                   string
	MinQuantity, UnitAmount int64
}
type PriceList struct {
	TenantID, ID, Currency, Status            string
	StartsAt                                  time.Time
	EndsAt                                    *time.Time
	Priority                                  int32
	Audience, CustomerGroupID, BuyerCompanyID string
	Entries                                   []Entry
}

// Request identity/audience fields come from the caller's authenticated context.
type Request struct {
	TenantID, SKUID, Currency       string
	Quantity                        int64
	At                              time.Time
	CustomerGroupID, BuyerCompanyID string
}
type Result struct {
	Unit, Line  money.Money
	PriceListID string
	Priority    int32
	MinQuantity int64
}

// Quote uses [start,end), priority descending, list ID ascending and tier descending.
// Currency filtering is explicit; this function neither authenticates nor converts.
func Quote(lists []PriceList, request Request) (Result, error) {
	if request.TenantID == "" || request.SKUID == "" || request.Quantity <= 0 || request.At.IsZero() {
		return Result{}, ErrInvalid
	}
	if err := (money.Money{Currency: request.Currency}).Validate(); err != nil {
		return Result{}, err
	}
	var best Result
	found := false
	ids := map[string]bool{}
	for _, list := range lists {
		if list.TenantID != request.TenantID {
			continue
		}
		if list.ID == "" || ids[list.ID] || (list.Status != "draft" && list.Status != "active" && list.Status != "archived") {
			return Result{}, ErrInvalid
		}
		ids[list.ID] = true
		if list.EndsAt != nil && !list.EndsAt.After(list.StartsAt) {
			return Result{}, ErrInvalid
		}
		applies := false
		switch list.Audience {
		case "all":
			if list.CustomerGroupID != "" || list.BuyerCompanyID != "" {
				return Result{}, ErrInvalid
			}
			applies = true
		case "customer_group":
			if list.CustomerGroupID == "" || list.BuyerCompanyID != "" {
				return Result{}, ErrInvalid
			}
			applies = request.CustomerGroupID == list.CustomerGroupID
		case "buyer_company":
			if list.BuyerCompanyID == "" || list.CustomerGroupID != "" {
				return Result{}, ErrInvalid
			}
			applies = request.BuyerCompanyID == list.BuyerCompanyID
		default:
			return Result{}, ErrInvalid
		}
		if err := (money.Money{Currency: list.Currency}).Validate(); err != nil {
			return Result{}, ErrInvalid
		}
		if !applies || list.Status != "active" || list.Currency != request.Currency || request.At.Before(list.StartsAt) || (list.EndsAt != nil && !request.At.Before(*list.EndsAt)) {
			continue
		}
		tiers := map[Entry]bool{}
		for _, entry := range list.Entries {
			// The database unique key excludes amount; disagreeing duplicates are invalid too.
			key := Entry{SKUID: entry.SKUID, MinQuantity: entry.MinQuantity}
			if entry.SKUID == "" || entry.MinQuantity <= 0 || entry.UnitAmount < 0 || tiers[key] {
				return Result{}, ErrInvalid
			}
			tiers[key] = true
			if entry.SKUID != request.SKUID || entry.MinQuantity > request.Quantity {
				continue
			}
			if !found || list.Priority > best.Priority || (list.Priority == best.Priority && (list.ID < best.PriceListID || (list.ID == best.PriceListID && entry.MinQuantity > best.MinQuantity))) {
				best = Result{Unit: money.Money{Amount: entry.UnitAmount, Currency: list.Currency}, PriceListID: list.ID, Priority: list.Priority, MinQuantity: entry.MinQuantity}
				found = true
			}
		}
	}
	if !found {
		return Result{}, ErrNoPrice
	}
	line, err := best.Unit.Mul(request.Quantity)
	if err != nil {
		return Result{}, err
	}
	best.Line = line
	return best, nil
}
