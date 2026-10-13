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
// Serving returns the host's tenant and the current serving body: body while
// now() < valid_until (or no boundary), else the fallback. body is nil when the current
// publication has no serving row (legacy publish outside the CMS write path).
func (s *Store) Serving(ctx context.Context, host string) (tenant string, body []byte, err error) {
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.tenant_id::text,
   CASE WHEN sv.valid_until IS NULL OR now()<sv.valid_until THEN sv.body ELSE sv.fallback_body END
   FROM app.site_domains d JOIN app.sites s ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id)
   LEFT JOIN app.storefront_serving sv ON (sv.tenant_id,sv.site_id,sv.publication_revision)=(s.tenant_id,s.id,s.publication_revision)
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND s.published_release_id IS NOT NULL`, host).Scan(&tenant, &body)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = contract.ErrNotFound
	} else if err != nil {
		err = contract.ErrUnavailable
	}
	return
}
func (s *Store) Current(ctx context.Context, host string) (siteID, releaseID, locale string, manifest, pages []byte, err error) {
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		// The lateral budget sums manifest+pinned document jsonb::text bytes (so also bounds
		// any single page) BEFORE jsonb_agg; over preBudget the CASE never runs the aggregate
		// and pages is NULL. service.Project still enforces the exact compact 1 MiB limit.
		return tx.QueryRow(ctx, `SELECT s.id::text,r.id::text,r.manifest->>'locale',r.manifest,
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
   AND octet_length(r.manifest::text)<=$2`, host, preBudget).Scan(&siteID, &releaseID, &locale, &manifest, &pages)
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
