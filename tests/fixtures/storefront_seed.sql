-- Disposable storefront sample, owned by C-09a. seed_storefront.sh opts in.
-- Inputs use runtime RLS; verifier and publisher alone confer public trust.
-- Products snapshot actual published catalog listings/revisions; prices remain release-time samples.
-- Reruns verify fixed ownership/content and never reassign an existing hostname.
BEGIN ISOLATION LEVEL READ COMMITTED;
SELECT pg_advisory_xact_lock(18000901::bigint);
CREATE TEMP TABLE sample_expected ON COMMIT DROP AS
SELECT n,
 ('c0900001-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS tenant,
 ('c0900002-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS org,
 ('c0900003-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS site,
 ('c0900004-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS home,
 ('c0900004-0000-4000-8001-'||lpad(n::text,12,'0'))::uuid AS about,
 ('c0900005-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS release,
 ('c0900005-0000-4000-8001-'||lpad(n::text,12,'0'))::uuid AS candidate,
 ('c0900006-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS draft,
 CASE n WHEN 1 THEN 'OpenSite sample shop' ELSE 'OTHER_TENANT_SECRET' END AS title,
 jsonb_build_object('version',1,'locale','en',
  'title',CASE n WHEN 1 THEN 'OpenSite sample shop' ELSE 'OTHER_TENANT_SECRET' END,
  'description','A published example of pages and products.',
  'navigation','[{"label":"Home","href":"/"},{"label":"About","href":"/about"},{"label":"Products","href":"/products"}]'::jsonb,
  'products','[{"id":"c0900007-0000-4000-8000-000000000001","slug":"canvas-tote","name":"Canvas tote","description":"A reusable cotton tote for daily essentials.","unit_amount":"2400","currency":"USD"},{"id":"c0900007-0000-4000-8000-000000000002","slug":"ceramic-mug","name":"Ceramic mug","description":"A simple mug for your morning routine.","unit_amount":"1800","currency":"USD"}]'::jsonb) AS manifest,
 '{"title":"Made for everyday use","description":"A published example of pages and products.","blocks":[{"kind":"heading","text":"Made for everyday use"},{"kind":"paragraph","text":"Browse our published collection and learn how this sample site is made."}]}'::jsonb AS home_doc,
 '{"title":"About this shop","description":"About this published shop.","blocks":[{"kind":"heading","text":"About this shop"},{"kind":"paragraph","text":"This page belongs to the published release."}]}'::jsonb AS about_doc
FROM generate_series(1,2) n;
DO $seed$
DECLARE e record; item jsonb; catalog_products jsonb; built_manifest jsonb; exists_fixture boolean; unpublished uuid := 'c0900003-0000-4000-8000-000000000003';
BEGIN
 FOR e IN SELECT * FROM sample_expected ORDER BY n LOOP
  SELECT EXISTS(SELECT FROM app.tenants WHERE id=e.tenant) INTO exists_fixture;
  IF exists_fixture AND NOT EXISTS(SELECT FROM app.tenants WHERE id=e.tenant AND name='C09 storefront fixture '||e.n) THEN
   RAISE EXCEPTION 'SAMPLE_TENANT_ID_OWNED_BY_OTHER';
  END IF;
  IF NOT exists_fixture THEN
   INSERT INTO app.tenants(id,name) VALUES(e.tenant,'C09 storefront fixture '||e.n);
   PERFORM set_config('app.tenant_id',e.tenant::text,true);
   EXECUTE 'SET LOCAL ROLE app_runtime';
   INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(e.tenant,e.org,'C09 sample organization');
   INSERT INTO app.sites(tenant_id,id,organization_id,name,default_locale) VALUES(e.tenant,e.site,e.org,e.title,'en');
   FOR item IN SELECT value FROM jsonb_array_elements(e.manifest->'products') LOOP
    INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status)
     VALUES(e.tenant,(item->>'id')::uuid,item->>'slug','ready');
    INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) VALUES
     (e.tenant,(item->>'id')::uuid,'en',1,item->>'name',jsonb_build_object('description',item->>'description')),
     (e.tenant,(item->>'id')::uuid,'en',2,'DRAFT_CATALOG_SECRET',jsonb_build_object('description','DRAFT_CATALOG_SECRET'));
    INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision)
     VALUES(e.tenant,e.site,(item->>'id')::uuid,'en',item->>'slug','published',1);
   END LOOP;
   INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status)
    VALUES(e.tenant,'c0900007-0000-4000-8000-000000000003','unpublished-sample','ready');
   INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo)
    VALUES(e.tenant,'c0900007-0000-4000-8000-000000000003','en',1,'UNPUBLISHED_CATALOG_SECRET','{"description":"UNPUBLISHED_CATALOG_SECRET"}');
   INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision)
    VALUES(e.tenant,e.site,'c0900007-0000-4000-8000-000000000003','en','unpublished-sample','unpublished',1);
   -- Mutable catalog pointers are resolved ONCE while building this immutable release.
   -- Only unit_amount/currency use sample price snapshots; no pricing write authority.
   SELECT coalesce(jsonb_agg(jsonb_build_object('id',p.id::text,'slug',l.slug,
    'name',c.title,'description',c.seo->>'description',
    'unit_amount',price.value->>'unit_amount','currency',price.value->>'currency') ORDER BY p.id),'[]'::jsonb)
    INTO catalog_products
    FROM app.product_listings l
    JOIN app.products p ON (p.tenant_id,p.id)=(l.tenant_id,l.product_id)
    JOIN app.product_content_revisions c ON (c.tenant_id,c.product_id,c.locale,c.revision)=
     (l.tenant_id,l.product_id,l.locale,l.published_content_revision)
    JOIN jsonb_array_elements(e.manifest->'products') price(value) ON (price.value->>'id')::uuid=p.id
    WHERE l.tenant_id=e.tenant AND l.site_id=e.site AND l.locale='en'
     AND l.status='published' AND p.lifecycle_status='ready';
   IF catalog_products IS DISTINCT FROM e.manifest->'products' THEN
    RAISE EXCEPTION 'SAMPLE_CATALOG_SNAPSHOT_MISMATCH';
   END IF;
   built_manifest:=jsonb_set(e.manifest,'{products}',catalog_products);
   INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(e.tenant,e.site,e.home,'home'),(e.tenant,e.site,e.about,'about');
   INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES
    (e.tenant,e.site,e.home,'en',1,e.home_doc),(e.tenant,e.site,e.about,'en',1,e.about_doc),
    (e.tenant,e.site,e.home,'en',2,'{"title":"DRAFT_REVISION_SECRET","blocks":[{"kind":"paragraph","text":"DRAFT_REVISION_SECRET"}]}');
   INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) VALUES
    (e.tenant,e.site,e.release,1,'c09-fixture-v1',built_manifest,sha256(convert_to(built_manifest::text,'UTF8'))),
    (e.tenant,e.site,e.candidate,2,'c09-fixture-v1','{"version":1,"locale":"en","title":"CANDIDATE_RELEASE_SECRET"}',sha256(convert_to('{"version":1,"locale":"en","title":"CANDIDATE_RELEASE_SECRET"}'::jsonb::text,'UTF8')));
   INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision) VALUES
    (e.tenant,e.site,e.release,'en','/',e.home,1),(e.tenant,e.site,e.release,'en','/about',e.about,1),
    (e.tenant,e.site,e.release,'en','/guides/setup',e.about,1),
    (e.tenant,e.site,e.candidate,'en','/',e.home,2);
   INSERT INTO app.site_drafts(tenant_id,site_id,id,base_release_id) VALUES(e.tenant,e.site,e.draft,e.release);
   INSERT INTO app.draft_pages(tenant_id,site_id,draft_id,page_id,locale,path,document)
    VALUES(e.tenant,e.site,e.draft,e.home,'en','/','{"title":"OPEN_DRAFT_SECRET"}');
   IF e.n=1 THEN
    INSERT INTO app.sites(tenant_id,id,organization_id,name) VALUES(e.tenant,unpublished,e.org,'UNPUBLISHED_SITE_SECRET');
   END IF;
   EXECUTE 'RESET ROLE';
   EXECUTE 'SET LOCAL ROLE app_domain_verifier';
   IF e.n=1 THEN
    INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES
     (e.tenant,e.site,'127.0.0.1','verified',now()),(e.tenant,e.site,'localhost','verified',now()),
     (e.tenant,e.site,'pending.c09.test','pending',NULL),(e.tenant,e.site,'disabled.c09.test','disabled',NULL),
     (e.tenant,unpublished,'unpublished.c09.test','verified',now());
   ELSE
    INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
     VALUES(e.tenant,e.site,'other.c09.test','verified',now());
   END IF;
   EXECUTE 'RESET ROLE';
   EXECUTE 'SET LOCAL ROLE app_publisher';
   PERFORM app.publish_release(e.tenant,e.site,e.release,NULL);
   EXECUTE 'RESET ROLE';
  END IF;
  -- Reruns check catalog ownership, exact content and listing pointer; never repair drift.
  FOR item IN SELECT value FROM jsonb_array_elements(e.manifest->'products') LOOP
   IF NOT EXISTS(SELECT FROM app.products WHERE tenant_id=e.tenant AND id=(item->>'id')::uuid
     AND product_code=item->>'slug' AND lifecycle_status='ready')
    OR NOT EXISTS(SELECT FROM app.product_content_revisions WHERE tenant_id=e.tenant
     AND product_id=(item->>'id')::uuid AND locale='en' AND revision=1 AND title=item->>'name'
     AND body_blocks='[]'::jsonb AND seo=jsonb_build_object('description',item->>'description'))
    OR NOT EXISTS(SELECT FROM app.product_content_revisions WHERE tenant_id=e.tenant
     AND product_id=(item->>'id')::uuid AND locale='en' AND revision=2 AND title='DRAFT_CATALOG_SECRET'
     AND seo='{"description":"DRAFT_CATALOG_SECRET"}'::jsonb)
    OR NOT EXISTS(SELECT FROM app.product_listings WHERE tenant_id=e.tenant AND site_id=e.site
     AND product_id=(item->>'id')::uuid AND locale='en' AND slug=item->>'slug'
     AND status='published' AND published_content_revision=1) THEN
    RAISE EXCEPTION 'SAMPLE_CATALOG_OWNERSHIP_OR_CONTENT_DRIFT';
   END IF;
  END LOOP;
  IF NOT EXISTS(SELECT FROM app.product_listings l JOIN app.product_content_revisions c
   ON (c.tenant_id,c.product_id,c.locale,c.revision)=(l.tenant_id,l.product_id,l.locale,l.published_content_revision)
   WHERE l.tenant_id=e.tenant AND l.site_id=e.site AND l.product_id='c0900007-0000-4000-8000-000000000003'
    AND l.locale='en' AND l.slug='unpublished-sample' AND l.status='unpublished'
    AND l.published_content_revision=1 AND c.title='UNPUBLISHED_CATALOG_SECRET') THEN
   RAISE EXCEPTION 'SAMPLE_UNPUBLISHED_CATALOG_WITNESS_DRIFT';
  END IF;
  -- An existing fixture must match every release-pinned public input.
  IF NOT EXISTS(SELECT FROM app.organizations WHERE tenant_id=e.tenant AND id=e.org AND display_name='C09 sample organization')
   OR NOT EXISTS(SELECT FROM app.sites WHERE tenant_id=e.tenant AND id=e.site AND organization_id=e.org AND name=e.title AND default_locale='en' AND published_release_id=e.release)
   OR NOT EXISTS(SELECT FROM app.site_releases WHERE tenant_id=e.tenant AND site_id=e.site AND id=e.release AND sealed AND manifest=e.manifest AND manifest_hash=sha256(convert_to(e.manifest::text,'UTF8')))
   OR NOT EXISTS(SELECT FROM app.site_releases WHERE tenant_id=e.tenant AND site_id=e.site AND id=e.candidate AND NOT sealed AND manifest->>'title'='CANDIDATE_RELEASE_SECRET')
   OR NOT EXISTS(SELECT FROM app.page_revisions WHERE tenant_id=e.tenant AND site_id=e.site AND page_id=e.home AND locale='en' AND revision=1 AND document=e.home_doc)
   OR NOT EXISTS(SELECT FROM app.page_revisions WHERE tenant_id=e.tenant AND site_id=e.site AND page_id=e.about AND locale='en' AND revision=1 AND document=e.about_doc)
   OR NOT EXISTS(SELECT FROM app.page_revisions WHERE tenant_id=e.tenant AND site_id=e.site AND page_id=e.home AND locale='en' AND revision=2 AND document->>'title'='DRAFT_REVISION_SECRET')
   OR NOT EXISTS(SELECT FROM app.site_drafts WHERE tenant_id=e.tenant AND site_id=e.site AND id=e.draft AND status='open' AND base_release_id=e.release)
   OR NOT EXISTS(SELECT FROM app.draft_pages WHERE tenant_id=e.tenant AND site_id=e.site AND draft_id=e.draft AND page_id=e.home AND document->>'title'='OPEN_DRAFT_SECRET')
   OR (SELECT count(*) FROM app.site_release_routes WHERE tenant_id=e.tenant AND site_id=e.site AND release_id=e.release)<>3
   OR NOT EXISTS(SELECT FROM app.site_release_routes WHERE tenant_id=e.tenant AND site_id=e.site AND release_id=e.release AND locale='en' AND path='/' AND page_id=e.home AND page_revision=1)
   OR NOT EXISTS(SELECT FROM app.site_release_routes WHERE tenant_id=e.tenant AND site_id=e.site AND release_id=e.release AND locale='en' AND path='/about' AND page_id=e.about AND page_revision=1)
   OR NOT EXISTS(SELECT FROM app.site_release_routes WHERE tenant_id=e.tenant AND site_id=e.site AND release_id=e.release AND locale='en' AND path='/guides/setup' AND page_id=e.about AND page_revision=1) THEN
   RAISE EXCEPTION 'SAMPLE_FIXTURE_DRIFT';
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM (VALUES
  ('127.0.0.1','verified','000000000001'),('localhost','verified','000000000001'),
  ('pending.c09.test','pending','000000000001'),('disabled.c09.test','disabled','000000000001'),
  ('unpublished.c09.test','verified','000000000003'),('other.c09.test','verified','000000000002')
 ) AS expected(host,status,suffix) WHERE NOT EXISTS(SELECT FROM app.site_domains d
  WHERE d.hostname=expected.host AND d.normalized_hostname=expected.host AND d.status=expected.status
   AND d.site_id=('c0900003-0000-4000-8000-'||expected.suffix)::uuid
   AND d.tenant_id=('c0900001-0000-4000-8000-'||CASE WHEN expected.suffix='000000000002' THEN expected.suffix ELSE '000000000001' END)::uuid
   AND (d.status<>'verified' OR d.verified_at IS NOT NULL)))
  OR NOT EXISTS(SELECT FROM app.sites WHERE id=unpublished AND tenant_id='c0900001-0000-4000-8000-000000000001' AND published_release_id IS NULL) THEN
  RAISE EXCEPTION 'SAMPLE_HOST_OWNERSHIP_OR_WITNESS_DRIFT';
 END IF;
END $seed$;
COMMIT;
\echo Storefront sample verified; current sealed release and negative witnesses ready.
