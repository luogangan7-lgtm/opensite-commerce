// Responsibility: unit-check live SKU pricing/availability attachment; caller: Go gate.
// Dependencies: service/contract/pricing; invariant: no price means not purchasable.
package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

func TestAttachSKUs(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	site := contract.Site{Products: []contract.Product{{ID: "P1", Currency: "USD"}, {ID: "p2", Currency: "EUR"}}}
	lists := []pricing.PriceList{
		{TenantID: "t", ID: "l1", Currency: "USD", Status: "active", Audience: "all", StartsAt: at.Add(-time.Hour), Priority: 1,
			Entries: []pricing.Entry{{SKUID: "s1", MinQuantity: 1, UnitAmount: 9007199254740993}, {SKUID: "s3", MinQuantity: 1, UnitAmount: 5}}},
		{TenantID: "t", ID: "l0", Currency: "USD", Status: "active", Audience: "all", StartsAt: at.Add(-time.Hour),
			Entries: []pricing.Entry{{SKUID: "s1", MinQuantity: 1, UnitAmount: 1}}},
	}
	live := Live{TenantID: "t", At: at, PriceLists: lists, SKUs: []LiveSKU{
		{ProductID: "p1", SKUID: "s1", Code: "TOTE-RED", Options: []contract.OptionValue{{Name: "color", Value: "Red"}}, Available: true},
		{ProductID: "p1", SKUID: "s2", Code: "TOTE-NOPRICE", Options: []contract.OptionValue{}},
		{ProductID: "p2", SKUID: "s3", Code: "MUG", Options: []contract.OptionValue{}}, // USD price only: EUR product has none
		{ProductID: "other", SKUID: "s9", Code: "X", Options: []contract.OptionValue{}},
	}}
	got, err := AttachSKUs(site, live)
	if err != nil {
		t.Fatal(err)
	}
	want := []contract.SKU{{SKUID: "s1", SKUCode: "TOTE-RED", OptionValues: []contract.OptionValue{{Name: "color", Value: "Red"}}, PriceMinor: "9007199254740993", Currency: "USD", Available: true}}
	if fmt.Sprint(got.Products[0].SKUs) != fmt.Sprint(want) || got.Products[1].SKUs == nil || len(got.Products[1].SKUs) != 0 {
		t.Fatalf("attached SKUs: %+v", got.Products)
	}
	if site.Products[0].SKUs != nil {
		t.Fatal("input site mutated")
	}
	many := Live{TenantID: "t", At: at, PriceLists: lists}
	for i := 0; i <= MaxSKUsPerProduct; i++ {
		many.SKUs = append(many.SKUs, LiveSKU{ProductID: "p1", SKUID: "s1", Code: "C", Options: []contract.OptionValue{}})
	}
	if _, err := AttachSKUs(site, many); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("unbounded SKUs accepted: %v", err)
	}
	if _, err := AttachSKUs(site, Live{TenantID: "t", At: at, PriceLists: lists, SKUs: []LiveSKU{{ProductID: "p1", SKUID: "s1", Code: ""}}}); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("invalid SKU accepted: %v", err)
	}
}
