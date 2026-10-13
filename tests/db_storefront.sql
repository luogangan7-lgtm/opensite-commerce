-- Responsibility: real public-role host/current/pinned visibility regression.
-- Dependencies: all forward migrations through storefront_read; caller: expendable PG gate.
-- Invariant: positive public page exists, drafts/history/other hosts never appear.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES ('storefront-public') RETURNING id AS tenant \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES (:'tenant','Public') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES (:'tenant',:'org','Public') RETURNING id AS site \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES (:'tenant',:'org','Other') RETURNING id AS other_site \gset
INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
 VALUES (:'tenant',:'site','storefront-public.example','verified',now()),(:'tenant',:'other_site','storefront-other.example','verified',now());
INSERT INTO app.site_domains(tenant_id,site_id,hostname) VALUES (:'tenant',:'site','storefront-unverified.example');
INSERT INTO app.pages(tenant_id,site_id,page_key) VALUES (:'tenant',:'site','about') RETURNING id AS page \gset
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES
 (:'tenant',:'site',:'page','en',1,'{"title":"PUBLIC_PAGE","description":"Public","blocks":[{"kind":"heading","text":"Public"}]}'),
 (:'tenant',:'site',:'page','en',2,'{"title":"DRAFT_SECRET","description":"Draft","blocks":[{"kind":"paragraph","text":"DRAFT_SECRET"}]}'),
 (:'tenant',:'site',:'page','en',3,'{"title":"PRIVATE_FIELD","description":"Private","blocks":[{"kind":"paragraph","text":"Public","internal":"SECRET"}]}'),
 (:'tenant',:'site',:'page','en',4,'{"title":"RAW_HTML","description":"Private","blocks":[{"kind":"raw_html","text":"<script>"}]}');
SELECT set_config('app.tenant_id',:'tenant',true);
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash) VALUES
 (:'tenant',:'site',1,'test','{"version":1,"locale":"en","title":"OLD_RELEASE","description":"old","navigation":[],"products":[]}',decode(repeat('11',32),'hex')) RETURNING id AS old_release \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash) VALUES
 (:'tenant',:'site',2,'test','{"version":1,"locale":"en","title":"CURRENT_RELEASE","description":"Public","navigation":[],"products":[]}',decode(repeat('22',32),'hex')) RETURNING id AS release \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash) VALUES
 (:'tenant',:'site',3,'test','{"version":1,"locale":"en","title":"UNSEALED_SECRET","description":"draft","navigation":[],"products":[]}',decode(repeat('33',32),'hex')) RETURNING id AS draft_release \gset
INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 VALUES (:'tenant',:'site',:'release','en','/about',:'page',1),
 (:'tenant',:'site',:'release','en','/private',:'page',3),
 (:'tenant',:'site',:'release','en','/raw',:'page',4);
SELECT app.seal_release(:'tenant',:'site',:'old_release');
SELECT app.publish_release(:'tenant',:'site',:'release',NULL);
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash) VALUES
 (:'tenant',:'other_site',1,'test','{"version":1,"locale":"en","title":"OTHER_HOST_SECRET","description":"other","navigation":[],"products":[]}',decode(repeat('44',32),'hex')) RETURNING id AS other_release \gset
SELECT app.publish_release(:'tenant',:'other_site',:'other_release',NULL);
SELECT set_config('test.public_tenant',:'tenant',true);
SELECT set_config('app.tenant_id','',true),set_config('app.public_hostname','storefront-public.example',true);
SET LOCAL ROLE app_domain_resolver;
DO $test$
BEGIN
 IF (SELECT count(manifest) FROM app.site_releases)<>1 OR
    (SELECT manifest->>'title' FROM app.site_releases)<>'CURRENT_RELEASE' THEN
  RAISE EXCEPTION 'STOREFRONT_CURRENT_RELEASE_SCOPE';
 END IF;
 IF (SELECT count(document) FROM app.page_revisions)<>1 OR
    (SELECT document->>'title' FROM app.page_revisions)<>'PUBLIC_PAGE' THEN
  RAISE EXCEPTION 'STOREFRONT_PINNED_REVISION_SCOPE';
 END IF;
 IF (SELECT count(path) FROM app.site_release_routes)<>3 THEN RAISE EXCEPTION 'STOREFRONT_ROUTE_SCOPE'; END IF;
 IF (SELECT count(id) FROM app.sites)<>1 THEN RAISE EXCEPTION 'STOREFRONT_HOST_SCOPE'; END IF;
 -- Spoofed tenant context cannot OR with public policies: no runtime membership.
 PERFORM set_config('app.tenant_id',current_setting('test.public_tenant'),true);
 IF (SELECT count(document) FROM app.page_revisions)<>1 THEN RAISE EXCEPTION 'STOREFRONT_TENANT_SPOOF'; END IF;
 PERFORM set_config('app.public_hostname','storefront-unverified.example',true);
 IF (SELECT count(id) FROM app.sites)<>0 OR (SELECT count(manifest) FROM app.site_releases)<>0 THEN RAISE EXCEPTION 'STOREFRONT_UNVERIFIED_HOST'; END IF;
 PERFORM set_config('app.public_hostname','unknown.example',true);
 IF (SELECT count(document) FROM app.page_revisions)<>0 THEN RAISE EXCEPTION 'STOREFRONT_UNKNOWN_HOST'; END IF;
 BEGIN PERFORM document FROM app.draft_pages; RAISE EXCEPTION 'STOREFRONT_DRAFT_ACL'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM product_code FROM app.products; RAISE EXCEPTION 'STOREFRONT_CATALOG_ACL'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 -- 028: stock is public only as the generated in_stock boolean, never as counts.
 BEGIN PERFORM on_hand FROM app.stock_levels; RAISE EXCEPTION 'STOREFRONT_STOCK_COUNT_ACL'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM reserved FROM app.stock_levels; RAISE EXCEPTION 'STOREFRONT_STOCK_COUNT_ACL'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM quantity FROM app.inventory_reservations; RAISE EXCEPTION 'STOREFRONT_RESERVATION_ACL'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.sites SET published_release_id=NULL; RAISE EXCEPTION 'STOREFRONT_PUBLIC_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $test$;
RESET ROLE;
-- Review ruL9x: route locales are unique case-insensitively per release/path, so
-- en/EN partitions can never be pinned (and later sealed) side by side.
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES
 (:'tenant',:'site',:'page','EN',1,'{"title":"CASE_ALIAS","description":"","blocks":[]}');
INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 VALUES (:'tenant',:'site',:'draft_release','en','/about',:'page',1);
SELECT set_config('test.t',:'tenant',true),set_config('test.s',:'site',true),
 set_config('test.r',:'draft_release',true),set_config('test.p',:'page',true);
DO $collision$
BEGIN
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
  VALUES (current_setting('test.t')::uuid,current_setting('test.s')::uuid,current_setting('test.r')::uuid,'EN','/about',current_setting('test.p')::uuid,1);
 RAISE EXCEPTION 'STOREFRONT_LOCALE_CASE_COLLISION';
EXCEPTION WHEN unique_violation THEN NULL;
END $collision$;
ROLLBACK;
SELECT 'PASS: storefront current sealed host-scoped pinned revision public role' AS result;
