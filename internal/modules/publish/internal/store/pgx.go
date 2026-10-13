//go:build pgx

// Package store reads one host-scoped snapshot from existing publish/CMS tables.
// Dependencies: pgxadapter and publish contract; caller: publish facade.
// Invariant: the serving row is preferred; legacy releases use one SQL snapshot, current sealed release, exact pinned page revisions;
// locale comes from the sealed manifest; byte budget checked before aggregation.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

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

// afterServingLookup is a test-only seam (nil in production) between the serving lookup and
// the legacy snapshot read inside the same read-only repeatable-read transaction.
var afterServingLookup func()

// Legacy is a sealed-release snapshot for a publication without a current serving row.
type Legacy struct {
	TenantID, SiteID, ReleaseID, Locale string
	Manifest, Pages                     []byte
}

// Serving reads, in ONE read-only repeatable-read snapshot, either the current serving
// representation (body while now() < valid_until and no invalidation is older than the 30 s
// grace, else the fallback) or, when the current
// publication has no serving row, the legacy snapshot whose tenant comes from the same
// row as its content. Tenant and content can therefore never pair across hostname moves.
func (s *Store) Serving(ctx context.Context, host string) (body []byte, legacy *Legacy, err error) {
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT true,
   CASE WHEN now()<sv.valid_until AND NOT EXISTS(SELECT FROM app.serving_invalidations i
     WHERE (i.tenant_id,i.site_id)=(sv.tenant_id,sv.site_id) AND i.created_at<now()-interval '30 seconds')
    THEN sv.body ELSE sv.fallback_body END
   FROM app.site_domains d JOIN app.sites s ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id)
   LEFT JOIN app.storefront_serving sv ON (sv.tenant_id,sv.site_id,sv.publication_revision)=(s.tenant_id,s.id,s.publication_revision)
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND s.published_release_id IS NOT NULL`, host).Scan(&found, &body); err != nil || body != nil {
			return err
		}
		if afterServingLookup != nil {
			afterServingLookup()
		}
		l, err := current(ctx, tx, host)
		legacy = &l
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = contract.ErrNotFound
	case errors.Is(err, contract.ErrUnavailable):
	case err != nil:
		err = contract.ErrUnavailable
	}
	return
}
func (s *Store) Current(ctx context.Context, host string) (siteID, releaseID, locale string, manifest, pages []byte, err error) {
	var l Legacy
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		l, e = current(ctx, tx, host)
		return e
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = contract.ErrNotFound
	case errors.Is(err, contract.ErrUnavailable):
	case err != nil:
		err = contract.ErrUnavailable
	}
	return l.SiteID, l.ReleaseID, l.Locale, l.Manifest, l.Pages, err
}

// current reads the sealed snapshot; the tenant comes from the same site row as the content.
func current(ctx context.Context, tx pgx.Tx, host string) (l Legacy, err error) {
	// The lateral budget sums manifest+pinned document jsonb::text bytes (so also bounds
	// any single page) BEFORE jsonb_agg; over preBudget the CASE never runs the aggregate
	// and pages is NULL. service.Project still enforces the exact compact 1 MiB limit.
	err = tx.QueryRow(ctx, `SELECT s.tenant_id::text,s.id::text,r.id::text,r.manifest->>'locale',r.manifest,
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
   AND octet_length(r.manifest::text)<=$2`, host, preBudget).Scan(&l.TenantID, &l.SiteID, &l.ReleaseID, &l.Locale, &l.Manifest, &l.Pages)
	if err == nil && l.Pages == nil {
		err = contract.ErrUnavailable // snapshot exceeds the response budget; nothing aggregated
	}
	return
}
