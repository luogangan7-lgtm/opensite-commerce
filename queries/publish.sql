-- Responsibility: parameterized controlled CMS calls; dependencies: controlled CMS forward migration.
-- Caller: publish pgx adapter in authorized tenant TX; actor from trusted context.
-- Invariants: no SQL outside adapter; C04 completion uses this same TX.
-- name: SetCMSActor :one
SELECT set_config('app.actor_kind',sqlc.arg(actor_kind)::text,true)::text AS actor_kind,
 set_config('app.actor_id',sqlc.arg(actor_id)::text,true)::text AS actor_id;
-- name: CMSCreateDraft :one
SELECT app.cms_create_draft(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,sqlc.arg(locale)::text,sqlc.arg(title)::text,sqlc.arg(description)::text)::jsonb AS result;
-- name: CMSReadDraft :one
SELECT app.cms_read_draft(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(draft_id)::uuid,sqlc.narg(expected_revision)::bigint)::jsonb AS result;
-- name: CMSApply :one
SELECT app.cms_apply(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(draft_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,sqlc.arg(expected_revision)::bigint,sqlc.arg(operation)::text,sqlc.arg(payload)::jsonb)::jsonb AS result;
-- name: CMSPublish :one
SELECT app.cms_publish(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(draft_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,sqlc.arg(expected_revision)::bigint,sqlc.narg(expected_release_id)::uuid,sqlc.arg(expected_publication_revision)::bigint,sqlc.arg(renderer_build_id)::text)::jsonb AS result;
-- name: CMSRestore :one
SELECT app.cms_restore(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,sqlc.narg(expected_release_id)::uuid,sqlc.arg(expected_publication_revision)::bigint,sqlc.arg(release_id)::uuid)::jsonb AS result;
-- name: CMSReleaseHistory :one
SELECT app.cms_release_history(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid)::jsonb AS result;

-- name: LoadCMSReleaseSnapshot :one
-- Restore validates this exact sealed snapshot with the public Project renderer
-- inside its mutation transaction before pointer/audit/receipt can commit.
SELECT r.site_id::text AS site_id,r.id::text AS release_id,(r.manifest->>'locale')::text AS locale,r.manifest,
 CASE WHEN budget.bytes<=2097152 THEN coalesce((SELECT jsonb_agg(page.document ORDER BY page.path) FROM (
  SELECT rr.path,jsonb_build_object('path',rr.path,'title',pr.document->'title','description',pr.document->'description','blocks',pr.document->'blocks') AS document
  FROM app.site_release_routes rr JOIN app.page_revisions pr
   ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
  WHERE (rr.tenant_id,rr.site_id,rr.release_id)=(r.tenant_id,r.site_id,r.id) AND lower(rr.locale)=lower(r.manifest->>'locale')
  ORDER BY rr.path LIMIT 101) page),'[]'::jsonb) END::jsonb AS pages
FROM app.site_releases r
CROSS JOIN LATERAL (SELECT octet_length(r.manifest::text)+coalesce(sum(octet_length(pr.document::text)+octet_length(rr.path)+256),0) AS bytes
 FROM app.site_release_routes rr JOIN app.page_revisions pr
  ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
 WHERE (rr.tenant_id,rr.site_id,rr.release_id)=(r.tenant_id,r.site_id,r.id) AND lower(rr.locale)=lower(r.manifest->>'locale')) budget
WHERE r.tenant_id=sqlc.arg(tenant_id)::uuid AND r.site_id=sqlc.arg(site_id)::uuid AND r.id=sqlc.arg(release_id)::uuid AND r.sealed;

-- name: ServingPublication :one
-- Storefront serving build (ADR_STOREFRONT_SERVING): locks the publication row first, so a
-- refresh waits for an in-flight publish, then reads epoch, DB clock and generation.
SELECT s.publication_revision,s.published_release_id::text AS release_id,now()::timestamptz AS at,
 coalesce((SELECT sv.serving_generation FROM app.storefront_serving sv WHERE sv.tenant_id=s.tenant_id AND sv.site_id=s.id),0)::bigint AS generation
FROM app.sites s WHERE s.tenant_id=sqlc.arg(tenant_id)::uuid AND s.id=sqlc.arg(site_id)::uuid AND s.published_release_id IS NOT NULL
FOR NO KEY UPDATE OF s;

-- name: ServingFolded :many
-- The folded set: invalidations visible before the build reads its inputs (ADR 2.6).
SELECT seq FROM app.serving_invalidations
WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND site_id=sqlc.arg(site_id)::uuid ORDER BY seq LIMIT 10000;

-- name: ServingDue :many
-- Refresh sweep for one tenant: published sites with no current serving row, a dirty row,
-- unfolded invalidations, or a row within 60 s of valid_until (backfill, invalidation and
-- max-lifetime refresh).
SELECT s.id::text AS site_id FROM app.sites s
LEFT JOIN app.storefront_serving sv ON (sv.tenant_id,sv.site_id)=(s.tenant_id,s.id)
WHERE s.tenant_id=sqlc.arg(tenant_id)::uuid AND s.published_release_id IS NOT NULL
 AND (sv.site_id IS NULL OR sv.publication_revision<>s.publication_revision OR sv.valid_until<now()+interval '60 seconds'
  OR EXISTS(SELECT FROM app.serving_invalidations i WHERE (i.tenant_id,i.site_id)=(s.tenant_id,s.id)))
ORDER BY s.id LIMIT sqlc.arg(row_limit)::integer;

-- name: ServingLiveCandidates :many
-- Publish: ready SKUs of ready released products (input scan budget via row_limit).
SELECT v.product_id::text AS product_id,v.id::text AS sku_id,v.sku,
 coalesce((SELECT jsonb_agg(jsonb_build_object('name',o.code,'value',ov.label) ORDER BY o.code)
  FROM app.variant_option_values vo
  JOIN app.product_options o ON (o.tenant_id,o.product_id,o.id)=(vo.tenant_id,vo.product_id,vo.option_id)
  JOIN app.product_option_values ov ON (ov.tenant_id,ov.product_id,ov.option_id,ov.id)=(vo.tenant_id,vo.product_id,vo.option_id,vo.value_id)
  WHERE (vo.tenant_id,vo.product_id,vo.variant_id)=(v.tenant_id,v.product_id,v.id)),'[]'::jsonb)::jsonb AS option_values
FROM app.product_variants v JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
WHERE v.tenant_id=sqlc.arg(tenant_id)::uuid AND v.product_id::text=ANY(sqlc.arg(product_ids)::text[])
 AND v.lifecycle_status='ready' AND p.lifecycle_status='ready'
ORDER BY v.product_id,v.sku,v.id LIMIT sqlc.arg(row_limit)::integer;

-- name: ServingSealedCandidates :many
-- Restore: the release's sealed membership in order, with live readiness.
SELECT k.product_id::text AS product_id,k.sku_id::text AS sku_id,k.sku_code,k.option_values,
 coalesce(v.lifecycle_status='ready' AND p.lifecycle_status='ready',false)::boolean AS ready
FROM app.site_release_skus k
LEFT JOIN app.product_variants v ON (v.tenant_id,v.id)=(k.tenant_id,k.sku_id)
LEFT JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
WHERE k.tenant_id=sqlc.arg(tenant_id)::uuid AND k.site_id=sqlc.arg(site_id)::uuid AND k.release_id=sqlc.arg(release_id)::uuid
ORDER BY k.ordinal;

-- name: ServingPriceRows :many
-- Quantity-1 rows of active audience=all lists that have not ended (window applied in Go).
SELECT l.id::text AS list_id,l.currency::text AS currency,l.status,l.priority,l.starts_at,l.ends_at,l.audience,
 e.sku_id::text AS sku_id,e.min_quantity,e.unit_amount
FROM app.price_entries e JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
WHERE e.tenant_id=sqlc.arg(tenant_id)::uuid AND e.sku_id::text=ANY(sqlc.arg(sku_ids)::text[]) AND e.min_quantity=1
 AND l.status='active' AND l.audience='all' AND (l.ends_at IS NULL OR l.ends_at>sqlc.arg(at)::timestamptz)
ORDER BY l.id,e.sku_id LIMIT sqlc.arg(row_limit)::integer;

-- name: ServingAvailability :many
SELECT sku_id::text AS sku_id,bool_or(on_hand>reserved)::boolean AS available
FROM app.stock_levels WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND sku_id::text=ANY(sqlc.arg(sku_ids)::text[])
GROUP BY sku_id;

-- name: SealReleaseSKUs :one
SELECT app.cms_seal_skus(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(release_id)::uuid,sqlc.arg(skus)::jsonb)::integer;

-- name: PutServing :one
SELECT app.cms_serving_put(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(release_id)::uuid,
 sqlc.arg(publication_revision)::bigint,sqlc.arg(expected_generation)::bigint,sqlc.arg(body)::bytea,
 sqlc.arg(fallback_body)::bytea,sqlc.arg(valid_until)::timestamptz,sqlc.arg(folded)::bigint[])::bigint;
