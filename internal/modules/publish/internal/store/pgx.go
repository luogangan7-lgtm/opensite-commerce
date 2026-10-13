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

// maxPriceRows: one winning row per SKU and currency, allowing four release currencies.
const maxPriceRows = maxSKURows * 4

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

// readLive scopes explicitly by the verified host's current sealed release AND relies on
// 028 RLS; it never reads stock counts, only the generated in_stock boolean.
func readLive(ctx context.Context, tx pgx.Tx, host string) (live service.Live, err error) {
	const scope = `FROM app.site_domains d JOIN app.sites st ON (st.tenant_id,st.id)=(d.tenant_id,d.site_id)
   JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(st.tenant_id,st.id,st.published_release_id)
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND r.sealed`
	if err = tx.QueryRow(ctx, `SELECT r.tenant_id::text,now() `+scope, host).Scan(&live.TenantID, &live.At); err != nil {
		return
	}
	rows, err := tx.Query(ctx, `SELECT v.product_id::text,v.id::text,v.sku,
   coalesce((SELECT jsonb_agg(jsonb_build_object('name',o.code,'value',ov.label) ORDER BY o.code)
     FROM app.variant_option_values vo
     JOIN app.product_options o ON (o.tenant_id,o.product_id,o.id)=(vo.tenant_id,vo.product_id,vo.option_id)
     JOIN app.product_option_values ov ON (ov.tenant_id,ov.product_id,ov.option_id,ov.id)=(vo.tenant_id,vo.product_id,vo.option_id,vo.value_id)
     WHERE (vo.tenant_id,vo.product_id,vo.variant_id)=(v.tenant_id,v.product_id,v.id)),'[]'::jsonb),
   coalesce((SELECT bool_or(sl.in_stock) FROM app.stock_levels sl WHERE (sl.tenant_id,sl.sku_id)=(v.tenant_id,v.id)),false)
   FROM app.product_variants v JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
   WHERE v.tenant_id=$1::uuid AND v.lifecycle_status='ready' AND p.lifecycle_status='ready'
   ORDER BY v.product_id,v.sku,v.id LIMIT $2`, live.TenantID, maxSKURows+1)
	if err != nil {
		return
	}
	for rows.Next() {
		var sku service.LiveSKU
		if err = rows.Scan(&sku.ProductID, &sku.SKUID, &sku.Code, &sku.Options, &sku.Available); err != nil {
			rows.Close()
			return
		}
		live.SKUs = append(live.SKUs, sku)
	}
	if err = rows.Err(); err != nil {
		return
	}
	if len(live.SKUs) > maxSKURows {
		return live, contract.ErrUnavailable
	}
	// Only the winning anonymous default price per SKU and currency (pricing.Quote precedence:
	// priority desc, then list id asc; UUID order equals lowercase text order), so the row
	// cap scales with SKUs, not with how many lists a tenant keeps. Quote re-checks the row.
	rows, err = tx.Query(ctx, `SELECT DISTINCT ON (e.sku_id,l.currency) l.id::text,l.currency::text,l.status,l.priority,l.starts_at,l.ends_at,l.audience,e.sku_id::text,e.min_quantity,e.unit_amount
   FROM app.price_entries e JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
   JOIN app.product_variants v ON (v.tenant_id,v.id)=(e.tenant_id,e.sku_id)
   WHERE e.tenant_id=$1::uuid AND e.min_quantity=1 AND l.status='active' AND l.audience='all'
    AND l.starts_at<=$2 AND (l.ends_at IS NULL OR $2<l.ends_at)
   ORDER BY e.sku_id,l.currency,l.priority DESC,l.id LIMIT $3`, live.TenantID, live.At, maxPriceRows+1)
	if err != nil {
		return
	}
	defer rows.Close()
	index := map[string]int{}
	count := 0
	for rows.Next() {
		var l pricing.PriceList
		var e pricing.Entry
		var starts, ends pgtype.Timestamptz
		if err = rows.Scan(&l.ID, &l.Currency, &l.Status, &l.Priority, &starts, &ends, &l.Audience, &e.SKUID, &e.MinQuantity, &e.UnitAmount); err != nil {
			return
		}
		// Infinite bounds are valid schema values: -infinity starts at the beginning of
		// time and infinity never ends (the SQL window already excluded the other cases).
		l.StartsAt = starts.Time
		if starts.InfinityModifier == pgtype.NegativeInfinity {
			l.StartsAt = time.Time{}
		}
		var endsAt *time.Time
		if ends.Valid && ends.InfinityModifier == pgtype.Finite {
			endsAt = &ends.Time
		}
		if count++; count > maxPriceRows {
			return live, contract.ErrUnavailable
		}
		i, ok := index[l.ID]
		if !ok {
			l.TenantID, l.EndsAt = live.TenantID, endsAt
			i = len(live.PriceLists)
			index[l.ID] = i
			live.PriceLists = append(live.PriceLists, l)
		}
		live.PriceLists[i].Entries = append(live.PriceLists[i].Entries, e)
	}
	err = rows.Err()
	return
}
