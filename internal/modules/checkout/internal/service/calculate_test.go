// Responsibility: independent calculator regression witnesses. Dependencies:
// checkout contract and real pure pricing/tax/shipping; no database substitutes.
package service

import (
	"math"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

const (
	tenant  = "11111111-1111-4111-8111-111111111111"
	sku     = "22222222-2222-4222-8222-222222222222"
	line    = "33333333-3333-4333-8333-333333333333"
	zone    = "44444444-4444-4444-8444-444444444444"
	rate    = "55555555-5555-4555-8555-555555555555"
	profile = "66666666-6666-4666-8666-666666666666"
	list    = "77777777-7777-4777-8777-777777777777"
	cartID  = "88888888-8888-4888-8888-888888888888"
)

func quoteFixture() (contract.Cart, contract.Rules, contract.ShippingAddress) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return contract.Cart{ID: cartID, Version: 1, Lines: []contract.CartLine{{ID: line, SKUID: sku, Quantity: 2}}}, contract.Rules{
		TenantID: tenant, Currency: "USD", At: at, ShippingProfileID: profile, SKUs: map[string]contract.SKU{sku: {ID: sku, Code: "SKU-1", TaxCategory: "standard", WeightG: 500}},
		PriceLists:    []pricing.PriceList{{TenantID: tenant, ID: list, Currency: "USD", Status: "active", StartsAt: at.Add(-time.Hour), Audience: "all", Entries: []pricing.Entry{{SKUID: sku, MinQuantity: 1, UnitAmount: 100}}}},
		Tax:           tax.Ruleset{Zones: []tax.Zone{{ID: zone, Locations: []tax.Destination{{Country: "US"}}}}, Categories: []tax.Category{{Code: "standard"}}, Rates: []tax.Rate{{ID: rate, ZoneID: zone, Category: "standard", BasisPoints: 1000, ValidFrom: at.Add(-time.Hour)}}},
		ShippingZones: []shipping.Zone{{ID: zone, Destinations: []shipping.Destination{{Country: "US"}}}}, ShippingRates: []shipping.Rate{{ID: rate, ProfileID: profile, ZoneID: zone, Method: "fixed", Currency: "USD", Amount: 50}},
	}, contract.ShippingAddress{Country: "US", Address1: "123 Test Street", City: "Test City", PostalCode: "00000"}
}
func TestCalculateUsesActualRules(t *testing.T) {
	c, r, a := quoteFixture()
	q, e := calculate(c, r, a)
	if e != nil || len(q.Lines) != 1 || q.Net != 200 || q.Tax != 20 || q.Shipping.Charge.Amount != 50 {
		t.Fatalf("quote %#v err %v", q, e)
	}
	if q.Lines[0].Price.Unit.Amount != 100 || q.Lines[0].Tax.Amounts.Gross != 220 {
		t.Fatal("did not calculate real price/tax")
	}
}
func TestCalculateRejectsUnsafeInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*contract.Cart, *contract.Rules, *contract.ShippingAddress)
	}{
		{"empty", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) { c.Lines = nil }},
		{"duplicate", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) {
			c.Lines = append(c.Lines, c.Lines[0])
		}},
		{"negative_quantity", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) { c.Lines[0].Quantity = -1 }},
		{"missing_metadata", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) { delete(r.SKUs, sku) }},
		{"overflow", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) {
			r.PriceLists[0].Entries[0].UnitAmount = math.MaxInt64
		}},
		{"foreign_price", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) {
			r.PriceLists[0].TenantID = "other"
		}},
		{"no_tax", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) { r.Tax.Rates = nil }},
		{"no_shipping", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) { r.ShippingRates = nil }},
		{"zero_payable", func(c *contract.Cart, r *contract.Rules, a *contract.ShippingAddress) {
			r.PriceLists[0].Entries[0].UnitAmount = 0
			r.ShippingRates[0].Amount = 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r, a := quoteFixture()
			tc.change(&c, &r, &a)
			_, e := calculate(c, r, a)
			if e == nil {
				t.Fatal("unsafe quote accepted")
			}
		})
	}
}
