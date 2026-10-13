// Responsibility: build the exact public wire body of a release (storefront serving row).
// Dependencies: publish contract, internal/pricing (approved Pricing Port), stdlib.
// Callers: publish store inside the publish/restore tenant transaction.
// Invariants: one pricing rule (anonymous, qty 1, release currency, list window,
// priority desc then list id asc); unpriced/non-ready SKUs are dropped BEFORE output
// caps; the 1 MiB budget is checked on exact bytes with refresh-variable fields at
// maximum width; bad catalog data rejects the build (422), never the live read.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

const (
	// MaxSKUsPerProduct and MaxSKUsPerSite are output caps on purchasable SKUs.
	MaxSKUsPerProduct = 100
	MaxSKUsPerSite    = 5000
	// MaxInputVariants and MaxInputPriceRows bound the rows a build may scan.
	MaxInputVariants  = 20000
	MaxInputPriceRows = 100000
	// WireBudget is the HTTP buffer limit for the complete response body.
	WireBudget = 1 << 20
	// maxPriceWidth is the widest price_minor a refresh can write (int64 max).
	maxPriceWidth = 19
	// MaxLifetime bounds every serving row: without a refresh it falls back after this.
	MaxLifetime = 15 * time.Minute
)

// ErrServingBounds rejects a build whose inputs or outputs exceed the bounds (HTTP 422).
var ErrServingBounds = fmt.Errorf("%w: storefront serving bounds", contract.ErrInvalid)

// ServingCandidate is one SKU considered for the body. Ready is the live lifecycle of the
// SKU and its product; Available is on-hand minus reserved > 0.
type ServingCandidate struct {
	ProductID, SKUID, Code string
	Options                []contract.OptionValue
	Ready, Available       bool
}

// ServingInput is read in the same tenant transaction as the publish or restore.
type ServingInput struct {
	TenantID   string
	Site       contract.Site
	At         time.Time
	Candidates []ServingCandidate
	PriceLists []pricing.PriceList // quantity-1 rows of active audience=all lists not yet ended
}

// ServingMember is sealed SKU membership (publish) in output order.
type ServingMember struct {
	ProductID, SKUID, Code string
	Options                []contract.OptionValue
}

// Serving is the built row: body and fallback are exact wire bytes (JSON plus newline);
// ValidUntil = min(next price boundary of a member SKU, At + MaxLifetime).
type Serving struct {
	Body, Fallback []byte
	ValidUntil     *time.Time
	Members        []ServingMember
}

// BuildServing prices candidates in one pass (lists split per SKU once) and encodes the body.
func BuildServing(ctx context.Context, in ServingInput) (Serving, error) {
	var out Serving
	index := map[string]int{}
	products := make([]contract.PublicProduct, len(in.Site.Products))
	for i, p := range in.Site.Products {
		products[i] = contract.PublicProduct{ID: p.ID, Slug: p.Slug, Name: p.Name, Description: p.Description, SKUs: []contract.SKU{}}
		key := strings.ToLower(p.ID)
		if _, dup := index[key]; dup {
			return out, ErrServingBounds // SKUs attach by product id: one view per id
		}
		index[key] = i
	}
	bySKU := map[string][]pricing.PriceList{}
	for _, list := range in.PriceLists {
		at := map[string]int{}
		for _, e := range list.Entries {
			lists := bySKU[e.SKUID]
			if i, ok := at[e.SKUID]; ok {
				lists[i].Entries = append(lists[i].Entries, e)
				continue
			}
			header := list
			header.Entries = []pricing.Entry{e}
			at[e.SKUID] = len(lists)
			bySKU[e.SKUID] = append(lists, header)
		}
	}
	extra, total := 0, 0 // extra: bytes a refresh may add at max field widths
	var boundary *time.Time
	for n, c := range in.Candidates {
		if n%256 == 0 && ctx.Err() != nil {
			return out, ctx.Err()
		}
		i, ok := index[strings.ToLower(c.ProductID)]
		if !ok || !c.Ready {
			continue
		}
		p := &products[i]
		currency := in.Site.Products[i].Currency // release currency of the product
		quote, err := pricing.Quote(bySKU[c.SKUID], pricing.Request{TenantID: in.TenantID, SKUID: c.SKUID, Currency: currency, Quantity: 1, At: in.At})
		if errors.Is(err, pricing.ErrNoPrice) {
			continue // not purchasable: never consumes an output cap
		}
		if err != nil || quote.Unit.Amount < 0 {
			return out, fmt.Errorf("%w: pricing data", ErrServingBounds)
		}
		if !text(c.SKUID, 128, true) || !text(c.Code, 255, true) || c.Options == nil || len(c.Options) > 20 {
			return out, ErrServingBounds
		}
		for _, o := range c.Options {
			if !text(o.Name, 255, true) || !text(o.Value, 255, false) {
				return out, ErrServingBounds
			}
		}
		if len(p.SKUs) == MaxSKUsPerProduct || total == MaxSKUsPerSite {
			return out, ErrServingBounds
		}
		price := strconv.FormatInt(quote.Unit.Amount, 10)
		p.SKUs = append(p.SKUs, contract.SKU{SKUID: c.SKUID, SKUCode: c.Code, OptionValues: c.Options, PriceMinor: price, Currency: quote.Unit.Currency, Available: c.Available})
		out.Members = append(out.Members, ServingMember{ProductID: p.ID, SKUID: c.SKUID, Code: c.Code, Options: c.Options})
		total++
		extra += maxPriceWidth - len(price)
		if c.Available {
			extra++ // "true" may become "false"
		}
		for _, l := range bySKU[c.SKUID] {
			if l.Currency != currency {
				continue
			}
			for _, t := range []*time.Time{&l.StartsAt, l.EndsAt} {
				if t != nil && t.After(in.At) && (boundary == nil || t.Before(*boundary)) {
					b := *t
					boundary = &b
				}
			}
		}
	}
	site := contract.PublicSite{TenantID: in.TenantID, SiteID: in.Site.SiteID, ReleaseID: in.Site.ReleaseID, Locale: in.Site.Locale, Title: in.Site.Title, Description: in.Site.Description, Navigation: in.Site.Navigation, Pages: in.Site.Pages, Products: products}
	body, err := wire(site)
	if err != nil {
		return out, err
	}
	if len(body)+extra > WireBudget {
		return out, fmt.Errorf("%w: wire budget", ErrServingBounds)
	}
	fallbackProducts := make([]contract.PublicProduct, len(products))
	for i, p := range products {
		p.SKUs = []contract.SKU{}
		fallbackProducts[i] = p
	}
	site.Products = fallbackProducts
	if out.Fallback, err = wire(site); err != nil {
		return out, err
	}
	until := in.At.Add(MaxLifetime)
	if boundary != nil && boundary.Before(until) {
		until = *boundary
	}
	out.Body, out.ValidUntil = body, &until
	return out, nil
}

// wire is the exact response encoding (encoding/json plus the Encoder's newline).
func wire(site contract.PublicSite) ([]byte, error) {
	raw, err := json.Marshal(site)
	if err != nil {
		return nil, ErrServingBounds
	}
	return append(raw, '\n'), nil
}
