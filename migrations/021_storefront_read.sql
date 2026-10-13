-- Responsibility: expose only a current sealed, host-scoped public release snapshot.
-- Dependencies: 005/006/009/010; caller: restricted publish Read Context service.
-- Invariants: FORCE RLS retained, acyclic visibility DAG, no draft/mutable catalog ACL.
-- The version-1 manifest is exclusively public locale/title/description/navigation/
-- products. Publication snapshots published catalog revisions; no live catalog ACL.
DROP POLICY public_verified_domains ON app.site_domains;
CREATE POLICY public_verified_domains ON app.site_domains FOR SELECT TO app_domain_resolver
 USING (status='verified' AND verified_at IS NOT NULL
  AND normalized_hostname=current_setting('app.public_hostname',true));
DROP POLICY public_published_sites ON app.sites;
CREATE POLICY public_published_sites ON app.sites FOR SELECT TO app_domain_resolver
 USING (published_release_id IS NOT NULL AND EXISTS (
  SELECT FROM app.site_domains d WHERE (d.tenant_id,d.site_id)=(sites.tenant_id,sites.id)));
GRANT SELECT (tenant_id,site_id,id,sealed,manifest) ON app.site_releases TO app_domain_resolver;
GRANT SELECT (tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 ON app.site_release_routes TO app_domain_resolver;
GRANT SELECT (tenant_id,site_id,page_id,locale,revision,document)
 ON app.page_revisions TO app_domain_resolver;
CREATE POLICY public_current_release ON app.site_releases FOR SELECT TO app_domain_resolver
 USING (sealed AND manifest->>'version'='1'
  AND manifest - ARRAY['version','locale','title','description','navigation','products']='{}'::jsonb
  AND jsonb_typeof(manifest->'title')='string' AND jsonb_typeof(manifest->'description')='string'
  AND jsonb_typeof(manifest->'locale')='string'
  AND CASE WHEN jsonb_typeof(manifest->'navigation')='array' THEN
   jsonb_array_length(manifest->'navigation')<=50 AND NOT EXISTS (
    SELECT FROM jsonb_array_elements(manifest->'navigation') n WHERE jsonb_typeof(n)<>'object'
     OR n - ARRAY['label','href']<>'{}'::jsonb OR jsonb_typeof(n->'label') IS DISTINCT FROM 'string'
     OR jsonb_typeof(n->'href') IS DISTINCT FROM 'string'
     OR n->>'href' !~ '^/([a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*)?$') ELSE false END
  AND CASE WHEN jsonb_typeof(manifest->'products')='array' THEN
   jsonb_array_length(manifest->'products')<=500 AND NOT EXISTS (
    SELECT FROM jsonb_array_elements(manifest->'products') p WHERE jsonb_typeof(p)<>'object'
     OR p - ARRAY['id','slug','name','description','unit_amount','currency']<>'{}'::jsonb
     OR jsonb_typeof(p->'id') IS DISTINCT FROM 'string' OR jsonb_typeof(p->'slug') IS DISTINCT FROM 'string'
     OR jsonb_typeof(p->'name') IS DISTINCT FROM 'string' OR jsonb_typeof(p->'description') IS DISTINCT FROM 'string'
     OR jsonb_typeof(p->'unit_amount') IS DISTINCT FROM 'string' OR jsonb_typeof(p->'currency') IS DISTINCT FROM 'string'
     OR p->>'slug' !~ '^[a-zA-Z0-9_-]+$' OR p->>'unit_amount' !~ '^(0|[1-9][0-9]{0,37})$'
     OR p->>'currency' !~ '^[A-Z]{3}$') ELSE false END
  AND EXISTS (SELECT FROM app.sites s WHERE (s.tenant_id,s.id,s.published_release_id)=(site_releases.tenant_id,site_releases.site_id,site_releases.id)));
CREATE POLICY public_current_routes ON app.site_release_routes FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.site_releases r
  WHERE (r.tenant_id,r.site_id,r.id)=(site_release_routes.tenant_id,site_release_routes.site_id,site_release_routes.release_id)));
CREATE POLICY public_pinned_revision ON app.page_revisions FOR SELECT TO app_domain_resolver
 USING (document - ARRAY['title','description','blocks']='{}'::jsonb
  AND jsonb_typeof(document->'title')='string' AND jsonb_typeof(document->'description')='string'
  AND CASE WHEN jsonb_typeof(document->'blocks')='array' THEN
   jsonb_array_length(document->'blocks')<=200 AND NOT EXISTS (
    SELECT FROM jsonb_array_elements(document->'blocks') b WHERE jsonb_typeof(b)<>'object'
     OR b - ARRAY['kind','text']<>'{}'::jsonb OR jsonb_typeof(b->'kind') IS DISTINCT FROM 'string'
     OR jsonb_typeof(b->'text') IS DISTINCT FROM 'string' OR b->>'kind' NOT IN ('heading','paragraph')) ELSE false END
  AND EXISTS (SELECT FROM app.site_release_routes rr
   WHERE (rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)=(page_revisions.tenant_id,page_revisions.site_id,page_revisions.page_id,page_revisions.locale,page_revisions.revision)));
