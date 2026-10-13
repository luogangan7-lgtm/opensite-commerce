// Responsibility: build and write the storefront serving row inside the publish/restore
// tenant transaction (docs/architecture/ADR_STOREFRONT_SERVING.md).
// Dependencies: generated db queries, publish service builder, internal/pricing types.
// Callers: WriteStore.Execute (publish_draft, restore_release).
// Invariants: same transaction as the release pointer move, so any error rolls back the
// publish; input scans are bounded; the write is a CAS on (publication, generation).
package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

// buildServing seals SKU membership (publish only) and writes the serving row for the
// release that the site's publication pointer now references.
func buildServing(ctx context.Context, q *db.Queries, tenant, site, release string, publish bool) error {
	view, err := projectSealed(ctx, q, tenant, site, release)
	if err != nil {
		return err
	}
	pub, err := q.ServingPublication(ctx, db.ServingPublicationParams{TenantID: cmsID(tenant), SiteID: cmsID(site)})
	if err != nil {
		return err
	}
	if pub.ReleaseID != release {
		return contract.ErrConflict
	}
	in := service.ServingInput{TenantID: tenant, Site: view, At: pub.At.Time}
	if publish {
		ids := make([]string, 0, len(view.Products))
		for _, p := range view.Products {
			ids = append(ids, strings.ToLower(p.ID))
		}
		rows, err := q.ServingLiveCandidates(ctx, db.ServingLiveCandidatesParams{TenantID: cmsID(tenant), ProductIds: ids, RowLimit: service.MaxInputVariants + 1})
		if err != nil {
			return err
		}
		if len(rows) > service.MaxInputVariants {
			return service.ErrServingBounds
		}
		for _, r := range rows {
			c := service.ServingCandidate{ProductID: r.ProductID, SKUID: r.SkuID, Code: r.Sku, Ready: true}
			if json.Unmarshal(r.OptionValues, &c.Options) != nil {
				return service.ErrServingBounds
			}
			in.Candidates = append(in.Candidates, c)
		}
	} else {
		rows, err := q.ServingSealedCandidates(ctx, db.ServingSealedCandidatesParams{TenantID: cmsID(tenant), SiteID: cmsID(site), ReleaseID: cmsID(release)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			c := service.ServingCandidate{ProductID: r.ProductID, SKUID: r.SkuID, Code: r.SkuCode, Ready: r.Ready}
			if json.Unmarshal(r.OptionValues, &c.Options) != nil {
				return service.ErrServingBounds
			}
			in.Candidates = append(in.Candidates, c)
		}
	}
	skus := make([]string, 0, len(in.Candidates))
	for _, c := range in.Candidates {
		skus = append(skus, c.SKUID)
	}
	if len(skus) > 0 {
		prices, err := q.ServingPriceRows(ctx, db.ServingPriceRowsParams{TenantID: cmsID(tenant), SkuIds: skus, At: pub.At, RowLimit: service.MaxInputPriceRows + 1})
		if err != nil {
			return err
		}
		if len(prices) > service.MaxInputPriceRows {
			return service.ErrServingBounds
		}
		in.PriceLists = priceLists(tenant, prices)
		stock, err := q.ServingAvailability(ctx, db.ServingAvailabilityParams{TenantID: cmsID(tenant), SkuIds: skus})
		if err != nil {
			return err
		}
		available := make(map[string]bool, len(stock))
		for _, s := range stock {
			available[s.SkuID] = s.Available
		}
		for i := range in.Candidates {
			in.Candidates[i].Available = available[in.Candidates[i].SKUID]
		}
	}
	out, err := service.BuildServing(ctx, in)
	if err != nil {
		return err
	}
	if publish {
		members := make([]map[string]any, 0, len(out.Members))
		for _, m := range out.Members {
			members = append(members, map[string]any{"product_id": strings.ToLower(m.ProductID), "sku_id": m.SKUID, "sku_code": m.Code, "option_values": m.Options})
		}
		raw, err := json.Marshal(members)
		if err != nil {
			return err
		}
		if _, err = q.SealReleaseSKUs(ctx, db.SealReleaseSKUsParams{TenantID: cmsID(tenant), SiteID: cmsID(site), ReleaseID: cmsID(release), Skus: raw}); err != nil {
			return err
		}
	}
	var until pgtype.Timestamptz
	if out.ValidUntil != nil {
		until = pgtype.Timestamptz{Time: *out.ValidUntil, Valid: true}
	}
	_, err = q.PutServing(ctx, db.PutServingParams{TenantID: cmsID(tenant), SiteID: cmsID(site), ReleaseID: cmsID(release),
		PublicationRevision: pub.PublicationRevision, ExpectedGeneration: pub.Generation, Body: out.Body, FallbackBody: out.Fallback, ValidUntil: until})
	return err
}

// priceLists groups price rows by list. Infinite bounds are valid schema values:
// -infinity starts at the beginning of time and infinity never ends.
func priceLists(tenant string, rows []db.ServingPriceRowsRow) []pricing.PriceList {
	index := map[string]int{}
	var lists []pricing.PriceList
	for _, r := range rows {
		if r.StartsAt.InfinityModifier == pgtype.Infinity {
			continue // starts at +infinity: never applies
		}
		i, ok := index[r.ListID]
		if !ok {
			l := pricing.PriceList{TenantID: tenant, ID: r.ListID, Currency: r.Currency, Status: r.Status, Priority: r.Priority, Audience: r.Audience, StartsAt: r.StartsAt.Time}
			if r.StartsAt.InfinityModifier == pgtype.NegativeInfinity {
				l.StartsAt = time.Time{}
			}
			if r.EndsAt.Valid && r.EndsAt.InfinityModifier == pgtype.Finite {
				ends := r.EndsAt.Time
				l.EndsAt = &ends
			}
			i = len(lists)
			index[r.ListID] = i
			lists = append(lists, l)
		}
		lists[i].Entries = append(lists[i].Entries, pricing.Entry{SKUID: r.SkuID, MinQuantity: r.MinQuantity, UnitAmount: r.UnitAmount})
	}
	return lists
}
