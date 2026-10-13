// Responsibility: unit gates for the storefront serving builder (ADR G2, G3, G9, valid_until).
// Dependencies: service/contract/pricing; caller: Go gate.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

var servingAt = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

func servingSite(products ...contract.Product) contract.Site {
	return contract.Site{SiteID: "s", ReleaseID: "r", Locale: "en", Title: "Shop", Navigation: []contract.Navigation{}, Pages: []contract.Page{}, Products: products}
}

func usdList(id string, priority int32, starts time.Time, ends *time.Time, entries ...pricing.Entry) pricing.PriceList {
	return pricing.PriceList{TenantID: "t", ID: id, Currency: "USD", Status: "active", Audience: "all", Priority: priority, StartsAt: starts, EndsAt: ends, Entries: entries}
}

func decodePublic(t *testing.T, body []byte) contract.PublicSite {
	t.Helper()
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatal("wire body must end with the encoder newline")
	}
	var site contract.PublicSite
	if err := json.Unmarshal(body, &site); err != nil {
		t.Fatal(err)
	}
	return site
}

// G9: the serving price is pricing.Quote for qty 1 in the release currency
// (priority desc, list id asc, window), and >2^53 stays an exact decimal string.
func TestServingPricingParity(t *testing.T) {
	later := servingAt.Add(48 * time.Hour)
	lists := []pricing.PriceList{
		usdList("l1", 1, servingAt.Add(-time.Hour), &later, pricing.Entry{SKUID: "s1", MinQuantity: 1, UnitAmount: 9007199254740993}),
		usdList("l0", 1, servingAt.Add(-time.Hour), nil, pricing.Entry{SKUID: "s1", MinQuantity: 1, UnitAmount: 7}), // same priority, lower id wins
		usdList("l9", 0, time.Time{}, nil, pricing.Entry{SKUID: "s1", MinQuantity: 1, UnitAmount: 1}),
		usdList("future", 9, servingAt.Add(10*time.Minute), nil, pricing.Entry{SKUID: "s1", MinQuantity: 1, UnitAmount: 3}),
	}
	in := ServingInput{TenantID: "t", At: servingAt, PriceLists: lists, Site: servingSite(contract.Product{ID: "P1", Currency: "USD"}, contract.Product{ID: "p2", Currency: "EUR"}),
		Candidates: []ServingCandidate{
			{ProductID: "p1", SKUID: "s1", Code: "TOTE", Options: []contract.OptionValue{{Name: "color", Value: "Red"}}, Ready: true, Available: true},
			{ProductID: "p2", SKUID: "s1", Code: "EUR-ONLY", Options: []contract.OptionValue{}, Ready: true},
		}}
	out, err := BuildServing(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	quote, err := pricing.Quote(lists, pricing.Request{TenantID: "t", SKUID: "s1", Currency: "USD", Quantity: 1, At: servingAt})
	if err != nil || quote.PriceListID != "l0" {
		t.Fatalf("reference quote: %+v %v", quote, err)
	}
	site := decodePublic(t, out.Body)
	want := []contract.SKU{{SKUID: "s1", SKUCode: "TOTE", OptionValues: []contract.OptionValue{{Name: "color", Value: "Red"}}, PriceMinor: fmt.Sprint(quote.Unit.Amount), Currency: "USD", Available: true}}
	if site.TenantID != "t" || !reflect.DeepEqual(site.Products[0].SKUs, want) || len(site.Products[1].SKUs) != 0 {
		t.Fatalf("serving SKUs: %+v", site.Products)
	}
	if out.ValidUntil == nil || !out.ValidUntil.Equal(servingAt.Add(10*time.Minute)) {
		t.Fatalf("valid_until must be the next price boundary: %v", out.ValidUntil)
	}
	in.PriceLists = lists[:3] // no future boundary: the max lifetime bounds the row
	if out, err = BuildServing(context.Background(), in); err != nil || !out.ValidUntil.Equal(servingAt.Add(MaxLifetime)) {
		t.Fatalf("valid_until must be capped by the max lifetime: %v %v", out.ValidUntil, err)
	}
	if fb := decodePublic(t, out.Fallback); len(fb.Products[0].SKUs) != 0 || fb.Products[0].SKUs == nil {
		t.Fatalf("fallback must keep products with skus []: %+v", fb.Products)
	}
	if len(out.Members) != 1 || out.Members[0].SKUID != "s1" {
		t.Fatalf("sealed membership: %+v", out.Members)
	}
}

// G3: unpriced and non-ready SKUs are dropped before the output caps; an output cap
// overrun is a typed bounds error (422 at publish), never a partial body.
func TestServingFiltersBeforeCaps(t *testing.T) {
	in := ServingInput{TenantID: "t", At: servingAt, Site: servingSite(contract.Product{ID: "p", Currency: "USD"})}
	priced := usdList("l", 0, servingAt.Add(-time.Hour), nil)
	for i := 0; i < 6000; i++ {
		in.Candidates = append(in.Candidates, ServingCandidate{ProductID: "p", SKUID: fmt.Sprintf("u%d", i), Code: strings.Repeat("X", 300), Options: make([]contract.OptionValue, 21), Ready: i%2 == 0})
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("s%d", i)
		in.Candidates = append(in.Candidates, ServingCandidate{ProductID: "p", SKUID: id, Code: id, Options: []contract.OptionValue{}, Ready: true})
		priced.Entries = append(priced.Entries, pricing.Entry{SKUID: id, MinQuantity: 1, UnitAmount: 5})
	}
	in.PriceLists = []pricing.PriceList{priced}
	out, err := BuildServing(context.Background(), in)
	if err != nil || len(decodePublic(t, out.Body).Products[0].SKUs) != 10 {
		t.Fatalf("unpriced rows consumed caps or failed the build: %v", err)
	}
	for i := 10; i <= MaxSKUsPerProduct; i++ {
		id := fmt.Sprintf("s%d", i)
		in.Candidates = append(in.Candidates, ServingCandidate{ProductID: "p", SKUID: id, Code: id, Options: []contract.OptionValue{}, Ready: true})
		in.PriceLists[0].Entries = append(in.PriceLists[0].Entries, pricing.Entry{SKUID: id, MinQuantity: 1, UnitAmount: 5})
	}
	if _, err := BuildServing(context.Background(), in); !errors.Is(err, ErrServingBounds) || !errors.Is(err, contract.ErrInvalid) {
		t.Fatalf("per-product cap overrun: %v", err)
	}
	bad := ServingInput{TenantID: "t", At: servingAt, Site: servingSite(contract.Product{ID: "p", Currency: "USD"}), PriceLists: []pricing.PriceList{usdList("l", 0, servingAt.Add(-time.Hour), nil, pricing.Entry{SKUID: "x", MinQuantity: 1, UnitAmount: 1})},
		Candidates: []ServingCandidate{{ProductID: "p", SKUID: "x", Code: strings.Repeat("X", 256), Options: []contract.OptionValue{}, Ready: true}}}
	if _, err := BuildServing(context.Background(), bad); !errors.Is(err, ErrServingBounds) {
		t.Fatalf("oversized priced SKU code accepted: %v", err)
	}
	dup := ServingInput{TenantID: "t", At: servingAt, Site: servingSite(contract.Product{ID: "p", Currency: "USD"}, contract.Product{ID: "P", Currency: "USD"})}
	if _, err := BuildServing(context.Background(), dup); !errors.Is(err, ErrServingBounds) {
		t.Fatalf("duplicate product ids accepted: %v", err)
	}
}

// G2: the budget counts exact wire bytes plus refresh-variable fields at max width.
func TestServingExactWireBudget(t *testing.T) {
	build := func(descLen int, available bool) ([]byte, error) {
		in := ServingInput{TenantID: "t", At: servingAt,
			Site:       servingSite(contract.Product{ID: "p", Currency: "USD", Description: strings.Repeat("x", descLen)}),
			PriceLists: []pricing.PriceList{usdList("l", 0, servingAt.Add(-time.Hour), nil, pricing.Entry{SKUID: "s", MinQuantity: 1, UnitAmount: 5})},
			Candidates: []ServingCandidate{{ProductID: "p", SKUID: "s", Code: "S", Options: []contract.OptionValue{}, Ready: true, Available: available}}}
		out, err := BuildServing(context.Background(), in)
		return out.Body, err
	}
	small, err := build(0, true)
	if err != nil || len(decodePublic(t, small).Products[0].SKUs) != 1 {
		t.Fatalf("fixture SKU must be priced: %s %v", small, err)
	}
	extra := 18 + 1 // "5" may become 19 digits; true may become false
	fit := WireBudget - len(small) - extra
	body, err := build(fit, true)
	if err != nil || len(body)+extra != WireBudget {
		t.Fatalf("payload exactly at budget rejected: %d %v", len(body)+extra, err)
	}
	if _, err := build(fit+1, true); !errors.Is(err, ErrServingBounds) {
		t.Fatalf("one byte over budget accepted: %v", err)
	}
	if body, err = build(fit, false); err != nil || len(body)+18 != WireBudget {
		t.Fatalf("available=false is already max width: %v", err)
	}
}

// Linear pricing at the input maxima (5,000 SKUs x 20,000 rows) and deadline honoring.
func TestServingBoundedWork(t *testing.T) {
	products := make([]contract.Product, 500)
	for i := range products {
		products[i] = contract.Product{ID: fmt.Sprintf("p%d", i), Currency: "USD"}
	}
	in := ServingInput{TenantID: "t", At: servingAt, Site: servingSite(products...)}
	for l := 0; l < 4; l++ {
		list := usdList(fmt.Sprintf("l%d", l), int32(l), servingAt.Add(-time.Hour), nil)
		for i := 0; i < 5000; i++ {
			list.Entries = append(list.Entries, pricing.Entry{SKUID: fmt.Sprintf("s%d", i), MinQuantity: 1, UnitAmount: int64(l*10 + 1)})
		}
		in.PriceLists = append(in.PriceLists, list)
	}
	for i := 0; i < 5000; i++ {
		in.Candidates = append(in.Candidates, ServingCandidate{ProductID: fmt.Sprintf("p%d", i%500), SKUID: fmt.Sprintf("s%d", i), Code: "C", Options: []contract.OptionValue{}, Ready: true})
	}
	start := time.Now()
	out, err := BuildServing(context.Background(), in)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("serving build at maxima took %v", elapsed)
	}
	if err != nil || len(out.Members) != 5000 || decodePublic(t, out.Body).Products[0].SKUs[0].PriceMinor != "31" {
		t.Fatalf("max-size build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildServing(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled build continued: %v", err)
	}
}
