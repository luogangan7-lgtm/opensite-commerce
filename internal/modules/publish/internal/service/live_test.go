// Responsibility: unit-check live SKU pricing/availability attachment; caller: Go gate.
// Dependencies: service/contract/pricing; invariant: no price means not purchasable.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		{ProductID: "p1", SKUID: "s2", Code: strings.Repeat("X", 300), Options: make([]contract.OptionValue, 21)}, // unpriced: skipped before shape checks
		{ProductID: "p2", SKUID: "s3", Code: "MUG", Options: []contract.OptionValue{}},                            // USD price only: EUR product has none
		{ProductID: "other", SKUID: "s9", Code: "X", Options: []contract.OptionValue{}},
	}}
	got, err := AttachSKUs(context.Background(), site, live)
	if err != nil {
		t.Fatal(err)
	}
	want := []contract.SKU{{SKUID: "s1", SKUCode: "TOTE-RED", OptionValues: []contract.OptionValue{{Name: "color", Value: "Red"}}, PriceMinor: "9007199254740993", Currency: "USD", Available: true}}
	if fmt.Sprint(got.Products[0].SKUs) != fmt.Sprint(want) || got.Products[1].SKUs == nil || len(got.Products[1].SKUs) != 0 {
		t.Fatalf("attached SKUs: %+v", got.Products)
	}
	if got.TenantID != "t" {
		t.Fatalf("tenant id not exposed: %q", got.TenantID)
	}
	if site.Products[0].SKUs != nil {
		t.Fatal("input site mutated")
	}
	many := Live{TenantID: "t", At: at, PriceLists: lists}
	for i := 0; i <= MaxSKUsPerProduct; i++ {
		many.SKUs = append(many.SKUs, LiveSKU{ProductID: "p1", SKUID: "s1", Code: "C", Options: []contract.OptionValue{}})
	}
	if _, err := AttachSKUs(context.Background(), site, many); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("unbounded SKUs accepted: %v", err)
	}
	if _, err := AttachSKUs(context.Background(), site, Live{TenantID: "t", At: at, PriceLists: lists, SKUs: []LiveSKU{{ProductID: "p1", SKUID: "s1", Code: ""}}}); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("invalid SKU accepted: %v", err)
	}
}

func TestAttachSKUsHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	site := contract.Site{Products: []contract.Product{{ID: "p", Currency: "USD"}}}
	if _, err := AttachSKUs(ctx, site, Live{TenantID: "t", At: time.Now(), SKUs: []LiveSKU{{ProductID: "p", SKUID: "s", Code: "C", Options: []contract.OptionValue{}}}}); !errors.Is(err, contract.ErrUnavailable) {
		t.Fatalf("canceled request still priced: %v", err)
	}
}

// At the store maxima (5,000 SKUs, 20,000 quantity-1 price rows) pricing stays linear.
func TestAttachSKUsBoundedWork(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	site := contract.Site{Products: make([]contract.Product, 500)}
	live := Live{TenantID: "t", At: at}
	for i := range site.Products {
		site.Products[i] = contract.Product{ID: fmt.Sprintf("p%d", i), Currency: "USD"}
	}
	for l := 0; l < 4; l++ {
		list := pricing.PriceList{TenantID: "t", ID: fmt.Sprintf("l%d", l), Currency: "USD", Status: "active", Audience: "all", StartsAt: at.Add(-time.Hour), Priority: int32(l)}
		for i := 0; i < 5000; i++ {
			list.Entries = append(list.Entries, pricing.Entry{SKUID: fmt.Sprintf("s%d", i), MinQuantity: 1, UnitAmount: int64(l*10 + 1)})
		}
		live.PriceLists = append(live.PriceLists, list)
	}
	for i := 0; i < 5000; i++ {
		live.SKUs = append(live.SKUs, LiveSKU{ProductID: fmt.Sprintf("p%d", i%500), SKUID: fmt.Sprintf("s%d", i), Code: "C", Options: []contract.OptionValue{}})
	}
	start := time.Now()
	got, err := AttachSKUs(context.Background(), site, live)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pricing 5,000 SKUs x 20,000 rows took %v", elapsed)
	}
	if err != nil || len(got.Products[0].SKUs) != 10 || got.Products[0].SKUs[0].PriceMinor != "31" {
		t.Fatalf("max-size pricing: %v %+v", err, got.Products[0].SKUs)
	}
}
