-- Responsibility: catalog locale content and listing persistence for CSV/staff API.
-- Caller: catalog shared helper; invariant: caller holds the product row lock.
-- name: LockCatalogContent :one
SELECT lifecycle_status FROM app.products WHERE tenant_id=$1 AND id=$2 FOR UPDATE;

-- name: ReadCatalogContent :one
SELECT jsonb_build_object('locale',sqlc.arg(locale)::text,'revision',coalesce(c.revision,0),
 'title',coalesce(c.title,''),'body_blocks',coalesce(c.body_blocks,'[]'::jsonb),'seo',coalesce(c.seo,'{}'::jsonb))::jsonb
FROM app.products p LEFT JOIN LATERAL(SELECT * FROM app.product_content_revisions
 WHERE tenant_id=p.tenant_id AND product_id=p.id AND locale=sqlc.arg(locale) ORDER BY revision DESC LIMIT 1) c ON true
WHERE p.tenant_id=sqlc.arg(tenant_id) AND p.id=sqlc.arg(product_id);

-- name: AppendCatalogContent :exec
INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,body_blocks,seo)
SELECT sqlc.arg(tenant_id),sqlc.arg(product_id),sqlc.arg(locale),coalesce(c.revision,0)+1,sqlc.arg(title),
 coalesce(sqlc.narg(body_blocks)::jsonb,c.body_blocks,'[]'::jsonb),coalesce(sqlc.narg(seo)::jsonb,c.seo,'{}'::jsonb)
FROM (SELECT 1) anchor LEFT JOIN LATERAL(SELECT revision,title,body_blocks,seo FROM app.product_content_revisions
 WHERE tenant_id=sqlc.arg(tenant_id) AND product_id=sqlc.arg(product_id) AND locale=sqlc.arg(locale) ORDER BY revision DESC LIMIT 1) c ON true
WHERE c.title IS DISTINCT FROM sqlc.arg(title)
 OR (sqlc.narg(body_blocks)::jsonb IS NOT NULL AND c.body_blocks IS DISTINCT FROM sqlc.narg(body_blocks)::jsonb)
 OR (sqlc.narg(seo)::jsonb IS NOT NULL AND c.seo IS DISTINCT FROM sqlc.narg(seo)::jsonb);

-- name: ReadCatalogListing :one
SELECT jsonb_build_object('site_id',s.id,'locale',sqlc.arg(locale)::text,'slug',coalesce(l.slug,''),
 'listed',coalesce(l.status='published',false),'content_revision',coalesce(l.published_content_revision,0),
 'revision',coalesce((SELECT max(revision) FROM app.product_content_revisions c WHERE c.tenant_id=p.tenant_id AND c.product_id=p.id AND c.locale=sqlc.arg(locale)),0))::jsonb
FROM app.products p JOIN app.sites s ON s.tenant_id=p.tenant_id
LEFT JOIN app.product_listings l ON l.tenant_id=p.tenant_id AND l.product_id=p.id AND l.site_id=s.id AND l.locale=sqlc.arg(locale)
WHERE p.tenant_id=sqlc.arg(tenant_id) AND p.id=sqlc.arg(product_id) AND s.id=sqlc.arg(site_id);

-- name: WriteCatalogListing :exec
INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision)
VALUES(sqlc.arg(tenant_id),sqlc.arg(site_id),sqlc.arg(product_id),sqlc.arg(locale),sqlc.arg(slug),
 CASE WHEN sqlc.arg(listed)::boolean THEN 'published' ELSE 'unpublished' END,sqlc.arg(content_revision)::bigint)
ON CONFLICT(tenant_id,site_id,product_id,locale) DO UPDATE SET slug=excluded.slug,status=excluded.status,published_content_revision=excluded.published_content_revision;
