//go:build pgx

// Package store reads one host-scoped snapshot from existing publish/CMS tables.
// Dependencies: pgxadapter and publish contract; caller: publish facade.
// Invariant: one repeatable-read snapshot, current sealed release, exact pinned page
// revisions; locale comes from the sealed manifest; byte budget checked before
// aggregation. Live SKUs/prices/stock are read in the same transaction (028 RLS).
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"time"
)

// maxSKURows bounds live SKU transfer: 500 products x MaxSKUsPerProduct cannot fit the
// 1 MiB response anyway, so more rows fail closed before projection.
const maxSKURows = 5000

// preBudget bounds the jsonb::text bytes summed before aggregation. jsonb::text only adds
// one space after each ':' and ',' and never escapes more than Go's json.Marshal, and every
// separator follows at least one value byte plus key quotes, so jsonb::text <= 1.5x the
// compact encoding. Twice service.Project's exact 1 MiB limit therefore never rejects a
// response Project would accept, while still capping aggregation/transfer at 2 MiB.
const preBudget = 2 << 20

type Store struct{ runner pgxadapter.Runner }

func Open(ctx context.Context, dsn string) (*Store, func(), error) {
	r, close, err := pgxadapter.OpenPublic(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return &Store{runner: r}, close, nil
}
func (s *Store) Current(ctx context.Context, host string) (siteID, releaseID, locale string, manifest, pages []byte, live service.Live, err error) {
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		// The lateral budget sums manifest+pinned document jsonb::text bytes (so also bounds
		// any single page) BEFORE jsonb_agg; over preBudget the CASE never runs the aggregate
		// and pages is NULL. service.Project still enforces the exact compact 1 MiB limit.
		if err := tx.QueryRow(ctx, `SELECT s.id::text,r.id::text,r.manifest->>'locale',r.manifest,
   CASE WHEN b.bytes<=$2 THEN coalesce((SELECT jsonb_agg(p.document ORDER BY p.path) FROM (
     SELECT rr.path,jsonb_build_object('path',rr.path,'title',pr.document->'title','description',pr.document->'description','blocks',pr.document->'blocks') AS document
     FROM app.site_release_routes rr JOIN app.page_revisions pr
     ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
     WHERE (rr.tenant_id,rr.site_id,rr.release_id)=(r.tenant_id,r.site_id,r.id) AND lower(rr.locale)=lower(r.manifest->>'locale')
     ORDER BY rr.path LIMIT 101) p), '[]'::jsonb) END
   FROM app.site_domains d JOIN app.sites s ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id)
   JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(s.tenant_id,s.id,s.published_release_id)
   CROSS JOIN LATERAL (SELECT octet_length(r.manifest::text)+coalesce(sum(octet_length(pr.document::text)),0) AS bytes
     FROM (SELECT pr.document FROM app.site_release_routes rr JOIN app.page_revisions pr
       ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
       WHERE (rr.tenant_id,rr.site_id,rr.release_id)=(r.tenant_id,r.site_id,r.id) AND lower(rr.locale)=lower(r.manifest->>'locale')
       ORDER BY rr.path LIMIT 101) pr) b
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND r.sealed
   AND octet_length(r.manifest::text)<=$2`, host, preBudget).Scan(&siteID, &releaseID, &locale, &manifest, &pages); err != nil || pages == nil {
			return err
		}
		var err error
		live, err = readLive(ctx, tx, host)
		return err
	})
	if err == nil && pages == nil {
		err = contract.ErrUnavailable // snapshot exceeds the response budget; nothing aggregated
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = contract.ErrNotFound
	} else if err != nil {
		err = contract.ErrUnavailable
	}
	return
}

// LiveSKUSQL ($1 tenant, $2 row cap, $3 release, $4 read time) is exported only so pgx
// tests can EXPLAIN it as the public role. One row per visible SKU of a released product,
// with its winning anonymous default price (pricing.Quote precedence: priority desc, then
// list id asc; UUID order equals lowercase text order) in the product's release currency,
// found by an indexed per-SKU lateral lookup. Quote re-checks the row in Go.
const LiveSKUSQL = `WITH released AS MATERIALIZED (
     SELECT DISTINCT lower(item->>'id') AS product_id,item->>'currency' AS currency
     FROM app.site_releases r CROSS JOIN LATERAL jsonb_array_elements(r.manifest->'products') item
     WHERE r.tenant_id=$1::uuid AND r.id=$3::uuid)
   SELECT v.product_id::text,v.id::text,v.sku,
   coalesce((SELECT jsonb_agg(jsonb_build_object('name',o.code,'value',ov.label) ORDER BY o.code)
     FROM app.variant_option_values vo
     JOIN app.product_options o ON (o.tenant_id,o.product_id,o.id)=(vo.tenant_id,vo.product_id,vo.option_id)
     JOIN app.product_option_values ov ON (ov.tenant_id,ov.product_id,ov.option_id,ov.id)=(vo.tenant_id,vo.product_id,vo.option_id,vo.value_id)
     WHERE (vo.tenant_id,vo.product_id,vo.variant_id)=(v.tenant_id,v.product_id,v.id)),'[]'::jsonb),
   coalesce((SELECT bool_or(sl.in_stock) FROM app.stock_levels sl WHERE (sl.tenant_id,sl.sku_id)=(v.tenant_id,v.id)),false),
   pr.list_id,pr.currency,pr.status,pr.priority,pr.starts_at,pr.ends_at,pr.audience,pr.min_quantity,pr.unit_amount
   FROM app.product_variants v JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
   JOIN released x ON x.product_id=v.product_id::text
   LEFT JOIN LATERAL (SELECT l.id::text AS list_id,l.currency::text AS currency,l.status,l.priority,l.starts_at,l.ends_at,l.audience,e.min_quantity,e.unit_amount
     FROM app.price_entries e JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
     WHERE e.tenant_id=v.tenant_id AND e.sku_id=v.id AND e.min_quantity=1 AND l.status='active' AND l.audience='all'
      AND l.starts_at<=$4 AND (l.ends_at IS NULL OR $4<l.ends_at) AND l.currency::text=x.currency
     ORDER BY l.priority DESC,l.id LIMIT 1) pr ON true
   WHERE v.tenant_id=$1::uuid AND v.lifecycle_status='ready' AND p.lifecycle_status='ready'
   ORDER BY v.product_id,v.sku,v.id LIMIT $2`

// readLive scopes explicitly by the verified host's current sealed release AND relies on
// 028 RLS; it never reads stock counts, only the generated in_stock boolean.
func readLive(ctx context.Context, tx pgx.Tx, host string) (live service.Live, err error) {
	const scope = `FROM app.site_domains d JOIN app.sites st ON (st.tenant_id,st.id)=(d.tenant_id,d.site_id)
   JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(st.tenant_id,st.id,st.published_release_id)
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND r.sealed`
	var releaseID string
	if err = tx.QueryRow(ctx, `SELECT r.tenant_id::text,r.id::text,now() `+scope, host).Scan(&live.TenantID, &releaseID, &live.At); err != nil {
		return
	}
	rows, err := tx.Query(ctx, LiveSKUSQL, live.TenantID, maxSKURows+1, releaseID, live.At)
	if err != nil {
		return
	}
	defer rows.Close()
	index := map[string]int{}
	for rows.Next() {
		var sku service.LiveSKU
		var listID, currency, status, audience *string
		var priority *int32
		var minQuantity, unitAmount *int64
		var starts, ends pgtype.Timestamptz
		if err = rows.Scan(&sku.ProductID, &sku.SKUID, &sku.Code, &sku.Options, &sku.Available,
			&listID, &currency, &status, &priority, &starts, &ends, &audience, &minQuantity, &unitAmount); err != nil {
			return
		}
		if live.SKUs = append(live.SKUs, sku); len(live.SKUs) > maxSKURows {
			return live, contract.ErrUnavailable
		}
		if listID == nil {
			continue // no anonymous default price: AttachSKUs omits the SKU
		}
		i, ok := index[*listID]
		if !ok {
			// Infinite bounds are valid schema values: -infinity starts at the beginning of
			// time and infinity never ends (the SQL window already excluded the other cases).
			l := pricing.PriceList{TenantID: live.TenantID, ID: *listID, Currency: *currency, Status: *status, Priority: *priority, Audience: *audience, StartsAt: starts.Time}
			if starts.InfinityModifier == pgtype.NegativeInfinity {
				l.StartsAt = time.Time{}
			}
			if ends.Valid && ends.InfinityModifier == pgtype.Finite {
				l.EndsAt = &ends.Time
			}
			i = len(live.PriceLists)
			index[*listID] = i
			live.PriceLists = append(live.PriceLists, l)
		}
		live.PriceLists[i].Entries = append(live.PriceLists[i].Entries, pricing.Entry{SKUID: sku.SKUID, MinQuantity: *minQuantity, UnitAmount: *unitAmount})
	}
	err = rows.Err()
	return
}
