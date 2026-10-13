-- Responsibility: parameterized controlled CMS calls; dependencies: migration025.
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
