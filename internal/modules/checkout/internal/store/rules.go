// Responsibility: load authorized calculation rules via SQLC, read-only.
// Dependencies: generated DB and pure calculator contracts; caller: checkout.
// Invariants: price/tax/shipping/time/currency/SKU code come from DB; category,
// weight and selected profile are explicit trusted configuration, never body data.
package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

type SKUMetadata struct {
	TaxCategory string
	WeightG     int64
}
type RuleMetadata struct {
	Profiles map[string]string
	SKUs     map[string]map[string]SKUMetadata
}
type Rules struct{ metadata RuleMetadata }

func NewRules(m RuleMetadata) *Rules {
	// Freeze configuration: caller mutations cannot change a checkout decision.
	c := RuleMetadata{Profiles: map[string]string{}, SKUs: map[string]map[string]SKUMetadata{}}
	for t, p := range m.Profiles {
		c.Profiles[t] = p
	}
	for t, items := range m.SKUs {
		c.SKUs[t] = map[string]SKUMetadata{}
		for id, v := range items {
			c.SKUs[t][id] = v
		}
	}
	return &Rules{metadata: c}
}
func (r *Rules) Load(ctx context.Context, tx pgx.Tx, actor contract.AuthorizedActor, cart contract.Cart, _ contract.ShippingAddress) (contract.Rules, error) {
	var out contract.Rules
	if !actor.Valid() {
		return out, contract.ErrForbidden
	}
	q, e := Queries(ctx, tx, actor.TenantID())
	if e != nil {
		return out, e
	}
	tenant := UUID(actor.TenantID())
	settings, e := q.GetCheckoutSettings(ctx, tenant)
	if e != nil {
		return out, MapError(e)
	}
	at, e := q.GetCheckoutServerTime(ctx)
	if e != nil {
		return out, MapError(e)
	}
	profile := r.metadata.Profiles[actor.TenantID()]
	if profile == "" {
		return out, contract.ErrUnavailable
	}
	out = contract.Rules{TenantID: actor.TenantID(), Currency: settings.Currency, At: at.Time, PricesIncludeTax: settings.PricesIncludeTax, ShippingProfileID: profile, SKUs: map[string]contract.SKU{}}
	priceLists := map[string]*pricing.PriceList{}
	for _, l := range cart.Lines {
		metadata, ok := r.metadata.SKUs[actor.TenantID()][l.SKUID]
		if !ok {
			return out, contract.ErrUnavailable
		}
		sku, e := q.GetCheckoutSKU(ctx, db.GetCheckoutSKUParams{TenantID: tenant, ID: UUID(l.SKUID)})
		if e != nil {
			return out, MapError(e)
		}
		out.SKUs[l.SKUID] = contract.SKU{ID: sku.ID.String(), Code: sku.Sku, TaxCategory: metadata.TaxCategory, WeightG: metadata.WeightG}
		rows, e := q.ListApplicablePriceEntries(ctx, db.ListApplicablePriceEntriesParams{TenantID: tenant, SkuID: UUID(l.SKUID), Quantity: l.Quantity, Currency: out.Currency, AtTime: at, CustomerGroupID: pgtype.UUID{}, BuyerCompanyID: pgtype.UUID{}})
		if e != nil {
			return out, MapError(e)
		}
		for _, v := range rows {
			id := v.PriceListID.String()
			p := priceLists[id]
			if p == nil {
				p = &pricing.PriceList{TenantID: actor.TenantID(), ID: id, Currency: v.Currency, Status: v.Status, Priority: v.Priority, StartsAt: v.StartsAt.Time, Audience: v.Audience}
				if v.EndsAt.Valid {
					t := v.EndsAt.Time
					p.EndsAt = &t
				}
				priceLists[id] = p
			}
			p.Entries = append(p.Entries, pricing.Entry{SKUID: l.SKUID, MinQuantity: v.MinQuantity, UnitAmount: v.UnitAmount})
		}
	}
	for _, p := range priceLists {
		out.PriceLists = append(out.PriceLists, *p)
	}
	locations, e := q.ListCheckoutTaxLocations(ctx, tenant)
	if e != nil {
		return out, MapError(e)
	}
	zones := map[string]int{}
	for _, l := range locations {
		id := l.ZoneID.String()
		i, ok := zones[id]
		if !ok {
			i = len(out.Tax.Zones)
			zones[id] = i
			out.Tax.Zones = append(out.Tax.Zones, tax.Zone{ID: id})
		}
		out.Tax.Zones[i].Locations = append(out.Tax.Zones[i].Locations, tax.Destination{Country: l.CountryCode, Subdivision: l.Subdivision})
	}
	categories, e := q.ListCheckoutTaxCategories(ctx, tenant)
	if e != nil {
		return out, MapError(e)
	}
	for _, v := range categories {
		out.Tax.Categories = append(out.Tax.Categories, tax.Category{Code: v.Code, IsExempt: v.IsExempt})
	}
	rates, e := q.ListCheckoutTaxRates(ctx, tenant)
	if e != nil {
		return out, MapError(e)
	}
	for _, v := range rates {
		rate := tax.Rate{ID: v.ID.String(), ZoneID: v.ZoneID.String(), Category: v.CategoryCode, BasisPoints: int64(v.RateBp), ValidFrom: v.ValidFrom.Time}
		if v.ValidUntil.Valid {
			t := v.ValidUntil.Time
			rate.ValidUntil = &t
		}
		out.Tax.Rates = append(out.Tax.Rates, rate)
	}
	shipLocations, e := q.ListCheckoutShippingLocations(ctx, db.ListCheckoutShippingLocationsParams{TenantID: tenant, ProfileID: UUID(profile)})
	if e != nil {
		return out, MapError(e)
	}
	shipZones := map[string]int{}
	for _, l := range shipLocations {
		id := l.ZoneID.String()
		i, ok := shipZones[id]
		if !ok {
			i = len(out.ShippingZones)
			shipZones[id] = i
			out.ShippingZones = append(out.ShippingZones, shipping.Zone{ID: id})
		}
		out.ShippingZones[i].Destinations = append(out.ShippingZones[i].Destinations, shipping.Destination{Country: l.CountryCode, Subdivision: l.SubdivisionCode})
	}
	shipRates, e := q.ListCheckoutShippingRates(ctx, db.ListCheckoutShippingRatesParams{TenantID: tenant, ProfileID: UUID(profile)})
	if e != nil {
		return out, MapError(e)
	}
	for _, v := range shipRates {
		rate := shipping.Rate{ID: v.ID.String(), ProfileID: v.ProfileID.String(), ZoneID: v.ZoneID.String(), Method: v.Method, Currency: v.Currency, Amount: v.Amount, MinWeightG: v.MinWeightG.Int64}
		if v.MaxWeightG.Valid {
			n := v.MaxWeightG.Int64
			rate.MaxWeightG = &n
		}
		if v.FreeOverAmount.Valid {
			n := v.FreeOverAmount.Int64
			rate.FreeOverAmount = &n
		}
		out.ShippingRates = append(out.ShippingRates, rate)
	}
	return out, nil
}
