//go:build pgx

// Package store reads one host-scoped snapshot from existing publish/CMS tables.
// Dependencies: pgxadapter and publish contract; caller: publish facade.
// Invariant: one SQL snapshot, current sealed release, exact pinned page revisions.
package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type Store struct{ runner pgxadapter.Runner }

func Open(ctx context.Context, dsn string) (*Store, func(), error) {
	r, close, err := pgxadapter.OpenPublic(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return &Store{runner: r}, close, nil
}
func (s *Store) Current(ctx context.Context, host string) (siteID, releaseID, locale string, manifest, pages []byte, err error) {
	err = s.runner.InPublicRead(ctx, host, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT s.id::text,r.id::text,s.default_locale,r.manifest,
   coalesce((SELECT jsonb_agg(p.document ORDER BY p.path) FROM (
     SELECT rr.path,jsonb_build_object('path',rr.path,'title',pr.document->'title','description',pr.document->'description','blocks',pr.document->'blocks') AS document
     FROM app.site_release_routes rr JOIN app.page_revisions pr
     ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
     WHERE (rr.tenant_id,rr.site_id,rr.release_id)=(r.tenant_id,r.site_id,r.id) AND rr.locale=s.default_locale
     AND octet_length(pr.document::text)<=50000 ORDER BY rr.path LIMIT 101) p), '[]'::jsonb)
   FROM app.site_domains d JOIN app.sites s ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id)
   JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(s.tenant_id,s.id,s.published_release_id)
   WHERE d.normalized_hostname=$1 AND d.status='verified' AND d.verified_at IS NOT NULL AND r.sealed
   AND octet_length(r.manifest::text)<=1048576`, host).Scan(&siteID, &releaseID, &locale, &manifest, &pages)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = contract.ErrNotFound
	} else if err != nil {
		err = contract.ErrUnavailable
	}
	return
}
