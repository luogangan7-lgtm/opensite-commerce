-- Cross-site release routes and publication pointer integrity, rollback-only.
BEGIN;
INSERT INTO app.tenants(name) VALUES ('release-FK') RETURNING id AS tenant_id \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES (:'tenant_id','Release Org')
 RETURNING id AS organization_id \gset
INSERT INTO app.sites(tenant_id,organization_id,name)
 VALUES (:'tenant_id',:'organization_id','Site 1') RETURNING id AS site_1 \gset
INSERT INTO app.sites(tenant_id,organization_id,name)
 VALUES (:'tenant_id',:'organization_id','Site 2') RETURNING id AS site_2 \gset
INSERT INTO app.pages(tenant_id,site_id,page_key)
 VALUES (:'tenant_id',:'site_1','home') RETURNING id AS page_1 \gset
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document)
 VALUES (:'tenant_id',:'site_1',:'page_1','en',1,'{}');
INSERT INTO app.pages(tenant_id,site_id,page_key)
 VALUES (:'tenant_id',:'site_2','home') RETURNING id AS page_2 \gset
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document)
 VALUES (:'tenant_id',:'site_2',:'page_2','en',1,'{}');
-- Tenant context so 010's fail-closed release guard sees the parent and the
-- negative case reaches the page_revision FK it is meant to exercise.
SELECT set_config('app.tenant_id',:'tenant_id',true);
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES (:'tenant_id',:'site_2',1,'test-renderer','{}',decode(repeat('61',32),'hex'))
 RETURNING id AS release_2 \gset
SELECT set_config('test.rel_tenant',:'tenant_id',true);
SELECT set_config('test.rel_site1',:'site_1',true);
SELECT set_config('test.rel_site2',:'site_2',true);
SELECT set_config('test.rel_page1',:'page_1',true);
SELECT set_config('test.rel_release2',:'release_2',true);
SELECT set_config('test.rel_page2',:'page_2',true);
DO $test$
DECLARE target text; got text;
BEGIN
 SELECT conname INTO STRICT target FROM pg_constraint
  WHERE conrelid='app.site_release_routes'::regclass AND confrelid='app.page_revisions'::regclass;
 -- Same-site positive: the guard and both FKs accept a legal route.
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 VALUES (current_setting('test.rel_tenant')::uuid,current_setting('test.rel_site2')::uuid,
  current_setting('test.rel_release2')::uuid,'en','/legal',current_setting('test.rel_page2')::uuid,1);
 BEGIN
  INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
  VALUES (current_setting('test.rel_tenant')::uuid,current_setting('test.rel_site2')::uuid,
   current_setting('test.rel_release2')::uuid,'en','/illegal',current_setting('test.rel_page1')::uuid,1);
  RAISE EXCEPTION 'cross-site route unexpectedly accepted';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS got = CONSTRAINT_NAME;
  IF got IS DISTINCT FROM target THEN
   RAISE EXCEPTION 'cross-site route rejected by % (%), not page_revision FK %',
    coalesce(got,'no constraint'),SQLERRM,target;
  END IF;
 END;
 BEGIN
  UPDATE app.sites SET published_release_id=current_setting('test.rel_release2')::uuid
   WHERE tenant_id=current_setting('test.rel_tenant')::uuid
    AND id=current_setting('test.rel_site1')::uuid;
  RAISE EXCEPTION 'cross-site release pointer unexpectedly accepted';
 EXCEPTION WHEN foreign_key_violation THEN NULL;
 END;
END $test$;
ROLLBACK;
SELECT 'PASS: cross-site publication FK isolation' AS result;
