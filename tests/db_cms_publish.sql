-- Responsibility: controlled CMS public-call gates on disposable PG18.
-- Dependencies: app CMS/publish 025 and catalog/pricing; caller: check_pg.
-- Invariants: actor fencing, draft CAS, immutable snapshot, restore ABA, rollback.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('C09B-CMS-GATE') RETURNING id AS tenant \gset
SELECT set_config('app.tenant_id',:'tenant',true);
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'tenant','CMS') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'tenant',:'org','CMS') RETURNING id AS site \gset
-- Published catalog uses actual ready representative SKU and active qty1/all price.
INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES
 (:'tenant','c09b1000-0000-4000-8000-000000000001','published','ready'),
 (:'tenant','c09b1000-0000-4000-8000-000000000002','unpriceable','ready'),
 (:'tenant','c09b1000-0000-4000-8000-000000000003','hidden','ready');
INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES
 (:'tenant','c09b2000-0000-4000-8000-000000000001','c09b1000-0000-4000-8000-000000000001','ARCHIVED','archived','archived'),
 (:'tenant','c09b2000-0000-4000-8000-000000000002','c09b1000-0000-4000-8000-000000000001','CHOSEN','chosen','ready'),
 (:'tenant','c09b2000-0000-4000-8000-000000000003','c09b1000-0000-4000-8000-000000000001','OTHER','other','ready');
INSERT INTO app.product_content_revisions(tenant_id,product_id,locale,revision,title,seo) VALUES
 (:'tenant','c09b1000-0000-4000-8000-000000000001','en',1,'Frozen catalog','{"description":"Published description"}'),
 (:'tenant','c09b1000-0000-4000-8000-000000000001','en',2,'LATER CATALOG','{}'),
 (:'tenant','c09b1000-0000-4000-8000-000000000002','en',1,'UNPRICEABLE','{}'),
 (:'tenant','c09b1000-0000-4000-8000-000000000003','en',1,'UNPUBLISHED SECRET','{}');
INSERT INTO app.product_listings(tenant_id,site_id,product_id,locale,slug,status,published_content_revision) VALUES
 (:'tenant',:'site','c09b1000-0000-4000-8000-000000000001','en','published','published',1),
 (:'tenant',:'site','c09b1000-0000-4000-8000-000000000002','en','unpriceable','published',1),
 (:'tenant',:'site','c09b1000-0000-4000-8000-000000000003','en','hidden','unpublished',1);
INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at,priority) VALUES
 (:'tenant','c09b3000-0000-4000-8000-000000000001','USD','active','2000-01-01',0),
 (:'tenant','c09b3000-0000-4000-8000-000000000002','USD','active','2000-01-01',10),
 (:'tenant','c09b3000-0000-4000-8000-000000000003','USD','active','2000-01-01',10);
INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES
 (:'tenant','c09b3000-0000-4000-8000-000000000001','c09b2000-0000-4000-8000-000000000002',1,900),
 (:'tenant','c09b3000-0000-4000-8000-000000000002','c09b2000-0000-4000-8000-000000000002',1,600),
 (:'tenant','c09b3000-0000-4000-8000-000000000003','c09b2000-0000-4000-8000-000000000002',1,700),
 (:'tenant','c09b3000-0000-4000-8000-000000000002','c09b2000-0000-4000-8000-000000000002',2,300),
 (:'tenant','c09b3000-0000-4000-8000-000000000001','c09b2000-0000-4000-8000-000000000001',1,1),
 (:'tenant','c09b3000-0000-4000-8000-000000000001','c09b2000-0000-4000-8000-000000000003',1,2);
INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status) VALUES(:'tenant','local','c09b-public',sha256('c09b-public'::bytea),1,'ready') RETURNING id AS blob \gset
INSERT INTO app.media_assets(tenant_id,id,blob_id,filename,mime_type,visibility) VALUES
 (:'tenant','c09b4000-0000-4000-8000-000000000001',:'blob','public.png','image/png','public'),
 (:'tenant','c09b4000-0000-4000-8000-000000000002',:'blob','private.png','image/png','private');
INSERT INTO app.tenants(name) VALUES('C09B-other') RETURNING id AS other_tenant \gset
INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status) VALUES(:'other_tenant','local','c09b-other',sha256('c09b-other'::bytea),1,'ready') RETURNING id AS other_blob \gset
INSERT INTO app.media_assets(tenant_id,id,blob_id,filename,mime_type,visibility) VALUES(:'other_tenant','c09b4000-0000-4000-8000-000000000003',:'other_blob','other.png','image/png','public');
SELECT set_config('app.tenant_id',:'tenant',true),set_config('app.actor_id','c09b0000-0000-4000-8000-000000000001',true),set_config('app.actor_kind','user',true);
SET LOCAL ROLE app_runtime;
SELECT app.cms_create_draft(:'tenant',:'site','user','c09b0000-0000-4000-8000-000000000001','en','CMS title','CMS description') AS draft \gset
SELECT set_config('test.cms_draft',(:'draft'::jsonb->>'draft_id'),true),set_config('test.cms_site',:'site',true);
DO $gate$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; s uuid:=current_setting('test.cms_site')::uuid; d uuid:=current_setting('test.cms_draft')::uuid;
 a uuid:=current_setting('app.actor_id')::uuid; result jsonb; p uuid; r uuid; r2 uuid; n integer; first_hash text; second_hash text;
BEGIN
 result:=app.cms_apply(t,s,d,'user',a,1,'save_page','{"page_key":"home","locale":"en","path":"/","document":{"title":"Home","description":"Description","blocks":[{"kind":"heading","text":"Visible"}]},"media_ids":["c09b4000-0000-4000-8000-000000000001"]}');
 IF (result->>'revision')::bigint<>2 THEN RAISE EXCEPTION 'CMS_GATE_REVISION'; END IF;
 p:=(result->'pages'->0->>'id')::uuid;
 IF jsonb_array_length(result->'products')<>1 OR result->'products'->0->>'unit_amount'<>'600' OR result->'products'->0->>'name'<>'Frozen catalog' THEN RAISE EXCEPTION 'CMS_GATE_CATALOG_PRICE_SELECTION'; END IF;
 BEGIN PERFORM app.cms_read_draft('c09b0000-0000-4000-8000-000000000099',s,d,NULL); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_TENANT'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,2,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document','{"title":"Home","blocks":[]}'::jsonb,'media_ids','["c09b4000-0000-4000-8000-000000000003"]'::jsonb)); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_CROSS_MEDIA'; EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,1,'set_navigation','{"navigation":[]}'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_STALE'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user','c09b0000-0000-4000-8000-000000000002',2,'set_navigation','{"navigation":[]}'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_ACTOR'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,2,'set_navigation','{"navigation":[{"label":"Danger","href":"https://bad.test"}]}'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_NAV'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,2,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document','{"title":"Home","blocks":[],"unsafe":true}'::jsonb,'media_ids','[]'::jsonb)); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_FIELDS'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,2,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document','{"title":"<script>bad</script>","blocks":[]}'::jsonb,'media_ids','[]'::jsonb)); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_HTML'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN UPDATE app.site_drafts SET revision=99 WHERE id=d; RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_DIRECT'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 result:=app.cms_apply(t,s,d,'user',a,2,'apply_operations','{"operations":[{"operation":"set_navigation","payload":{"navigation":[{"label":"Home","href":"/"},{"label":"Published product","href":"/products/published"}]}},{"operation":"set_metadata","payload":{"locale":"en","title":"Frozen title","description":"Frozen description"}}]}');
 IF (result->>'revision')::bigint<>3 THEN RAISE EXCEPTION 'CMS_GATE_BATCH_ONE_REVISION'; END IF;
 result:=app.cms_publish(t,s,d,'user',a,3,NULL,0,'cms-gate-v1'); r:=(result->>'release_id')::uuid; first_hash:=result->>'manifest_hash';
 IF NOT EXISTS(SELECT FROM app.site_release_media WHERE tenant_id=t AND site_id=s AND release_id=r AND media_id='c09b4000-0000-4000-8000-000000000001') THEN RAISE EXCEPTION 'CMS_GATE_MEDIA_PIN'; END IF;
 IF (result->>'publication_revision')::bigint<>1 THEN RAISE EXCEPTION 'CMS_GATE_PUBLISH_REVISION'; END IF;
 BEGIN PERFORM app.cms_publish(t,s,d,'user',a,3,NULL,0,'cms-gate-v1'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_CLOSED'; EXCEPTION WHEN serialization_failure OR object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,3,'set_navigation','{"navigation":[]}'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_CLOSED_WRITE'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.page_revisions SET document='{}' WHERE site_id=s; RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_REVISION_UPDATE'; EXCEPTION WHEN insufficient_privilege OR object_not_in_prerequisite_state THEN NULL; END;
 result:=app.cms_create_draft(t,s,'user',a,'en','Frozen title','Frozen description'); d:=(result->>'draft_id')::uuid;
 result:=app.cms_apply(t,s,d,'user',a,1,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document','{"title":"Home","description":"Description","blocks":[{"kind":"heading","text":"Different content"}]}'::jsonb,'media_ids','["c09b4000-0000-4000-8000-000000000001"]'::jsonb));
 result:=app.cms_publish(t,s,d,'user',a,2,r,1,'cms-gate-v1'); r2:=(result->>'release_id')::uuid; second_hash:=result->>'manifest_hash';
 IF (SELECT manifest FROM app.site_releases WHERE id=r AND tenant_id=t AND site_id=s) IS DISTINCT FROM (SELECT manifest FROM app.site_releases WHERE id=r2 AND tenant_id=t AND site_id=s) THEN RAISE EXCEPTION 'CMS_GATE_HASH_TEST_MANIFEST_DRIFT'; END IF;
 IF first_hash=second_hash THEN RAISE EXCEPTION 'CMS_GATE_HASH_IGNORED_PAGE_CONTENT'; END IF;
 result:=app.cms_restore(t,s,'user',a,r2,2,r);
 IF result->>'manifest_hash'<>first_hash THEN RAISE EXCEPTION 'CMS_GATE_RESTORE_HASH'; END IF;
 IF (result->>'release_id')::uuid<>r OR (result->>'publication_revision')::bigint<>3 THEN RAISE EXCEPTION 'CMS_GATE_RESTORE'; END IF;
 BEGIN PERFORM app.cms_restore(t,s,'user',a,r,1,r2); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_ABA'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 result:=app.cms_create_draft(t,s,'user',a,'en','Unsafe media','Unsafe media'); d:=(result->>'draft_id')::uuid;
 result:=app.cms_apply(t,s,d,'user',a,1,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document','{"title":"Home","blocks":[]}'::jsonb,'media_ids','["c09b4000-0000-4000-8000-000000000002"]'::jsonb));
 BEGIN PERFORM app.cms_publish(t,s,d,'user',a,2,r,3,'cms-gate-v1'); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_UNSAFE_MEDIA'; EXCEPTION WHEN check_violation THEN NULL; END;
 IF (SELECT status FROM app.site_drafts WHERE tenant_id=t AND site_id=s AND id=d)<>'open' OR (SELECT publication_revision FROM app.sites WHERE tenant_id=t AND id=s)<>3 THEN RAISE EXCEPTION 'CMS_GATE_FAILED_PUBLISH_ROLLBACK'; END IF;
 result:=app.cms_apply(t,s,d,'user',a,2,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document',jsonb_build_object('title',repeat('x',300),'description','','blocks','[]'::jsonb),'media_ids','[]'::jsonb));
 IF result->'pages'->0->'document'->>'title'<>repeat('x',300) THEN RAISE EXCEPTION 'CMS_GATE_300_TITLE'; END IF;
 BEGIN PERFORM app.cms_apply(t,s,d,'user',a,3,'save_page',jsonb_build_object('id',p,'page_key','home','locale','en','path','/','document',jsonb_build_object('title',repeat('x',301),'blocks','[]'::jsonb),'media_ids','[]'::jsonb)); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_301_TITLE'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 SELECT count(*) INTO n FROM app.site_releases WHERE tenant_id=t AND site_id=s;
 IF n<>2 THEN RAISE EXCEPTION 'CMS_GATE_RESTORE_COPIED'; END IF;
 IF (SELECT count(*) FROM app.audit_events WHERE tenant_id=t AND operation IN ('cms.publish','cms.restore'))<>3 THEN RAISE EXCEPTION 'CMS_GATE_AUDIT'; END IF;
 IF NOT EXISTS(SELECT FROM app.site_release_routes rr JOIN app.page_revisions pr ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision) WHERE rr.release_id=r AND pr.document->>'title'='Home') THEN RAISE EXCEPTION 'CMS_GATE_FROZEN_PAGE'; END IF;
END $gate$;
RESET ROLE;
DO $immutable$ BEGIN
 UPDATE app.product_listings SET published_content_revision=2 WHERE site_id=current_setting('test.cms_site')::uuid AND product_id='c09b1000-0000-4000-8000-000000000001';
 UPDATE app.price_entries SET unit_amount=99999 WHERE tenant_id=current_setting('app.tenant_id')::uuid;
 IF EXISTS(SELECT FROM app.site_releases WHERE site_id=current_setting('test.cms_site')::uuid AND sealed AND (manifest->'products'->0->>'name'<>'Frozen catalog' OR manifest->'products'->0->>'unit_amount'<>'600')) THEN RAISE EXCEPTION 'CMS_GATE_CATALOG_NOT_FROZEN'; END IF;
 BEGIN UPDATE app.page_revisions SET document='{}' WHERE site_id=current_setting('test.cms_site')::uuid; RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_PRIVILEGED_MUTATION'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN DELETE FROM app.page_revisions WHERE site_id=current_setting('test.cms_site')::uuid; RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_PRIVILEGED_DELETE'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.site_releases SET manifest='{}' WHERE site_id=current_setting('test.cms_site')::uuid AND sealed; RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_RELEASE_MUTATION'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $immutable$;
ROLLBACK;
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT set_config('app.tenant_id','c09b0000-0000-4000-8000-000000000099',true);
SET LOCAL ROLE app_runtime;
DO $rr$ BEGIN
 BEGIN PERFORM app.cms_read_draft('c09b0000-0000-4000-8000-000000000099','c09b0000-0000-4000-8000-000000000098','c09b0000-0000-4000-8000-000000000097',NULL); RAISE EXCEPTION 'CMS_GATE_RR_READ_NOT_FOUND'; EXCEPTION WHEN no_data_found THEN NULL; END;
END $rr$;
ROLLBACK;
-- A legacy release may publish multiple paths for one immutable page revision.
BEGIN;
INSERT INTO app.tenants(name) VALUES('C09B-alias-gate') RETURNING id AS tenant \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'tenant','Aliases') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'tenant',:'org','Aliases') RETURNING id AS site \gset
INSERT INTO app.pages(tenant_id,site_id,page_key) VALUES(:'tenant',:'site','home') RETURNING id AS page \gset
INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document) VALUES(:'tenant',:'site',:'page','en',1,'{"title":"Alias home","blocks":[]}');
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(:'tenant',:'site',1,'alias-gate','{"version":1,"locale":"en","title":"Alias","description":"","navigation":[{"label":"Alias","href":"/alias"}],"products":[]}',sha256('alias'::bytea)) RETURNING id AS release \gset
SELECT set_config('app.tenant_id',:'tenant',true);
INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision) VALUES
 (:'tenant',:'site',:'release','en','/',:'page',1),(:'tenant',:'site',:'release','en','/alias',:'page',1);
SELECT set_config('app.tenant_id',:'tenant',true),set_config('app.actor_id','c09b0000-0000-4000-8000-000000000001',true),set_config('app.actor_kind','user',true);
SELECT app.publish_release(:'tenant',:'site',:'release',NULL);
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(:'tenant',:'site',2,'legacy-gate','{"locale":"en"}',sha256('legacy'::bytea)) RETURNING id AS incompatible \gset
SELECT app.seal_release(:'tenant',:'site',:'incompatible');
SELECT set_config('test.cms_incompatible',:'incompatible',true);
SELECT set_config('test.cms_site',:'site',true),set_config('test.cms_release',:'release',true);
SET LOCAL ROLE app_runtime;
DO $aliases$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; s uuid:=current_setting('test.cms_site')::uuid;
 a uuid:=current_setting('app.actor_id')::uuid; d jsonb; result jsonb; page_id uuid; page_body jsonb;
BEGIN
 BEGIN PERFORM app.cms_restore(t,s,'user',a,current_setting('test.cms_release')::uuid,1,current_setting('test.cms_incompatible')::uuid);
  RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_MISSING_VERSION'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 d:=app.cms_create_draft(t,s,'user',a,'en','Alias','');
 IF jsonb_array_length(d->'pages')<>2 OR NOT EXISTS(SELECT FROM jsonb_array_elements(d->'pages') p WHERE p->>'path'='/alias')
  OR d->'navigation'->0->>'href'<>'/alias' THEN RAISE EXCEPTION 'CMS_GATE_ALIAS_CLONE'; END IF;
 result:=app.cms_publish(t,s,(d->>'draft_id')::uuid,'user',a,1,current_setting('test.cms_release')::uuid,1,'alias-gate');
 IF (SELECT count(*) FROM app.site_release_routes WHERE tenant_id=t AND site_id=s AND release_id=(result->>'release_id')::uuid)<>2 THEN RAISE EXCEPTION 'CMS_GATE_ALIAS_PUBLISH'; END IF;
 d:=app.cms_create_draft(t,s,'user',a,'en','Alias','');
 IF jsonb_array_length(d->'pages')<>2 THEN RAISE EXCEPTION 'CMS_GATE_ALIAS_RECLONE'; END IF;
 result:=app.cms_apply(t,s,(d->>'draft_id')::uuid,'user',a,1,'save_page','{"page_key":"remove","locale":"en","path":"/remove","document":{"title":"Remove","blocks":[]},"media_ids":[]}');
 SELECT (p->>'id')::uuid INTO page_id FROM jsonb_array_elements(result->'pages') p WHERE p->>'path'='/remove';
 page_body:=jsonb_build_object('id',page_id,'page_key','remove','locale','en','path','/remove','document',jsonb_build_object('title','Remove','blocks','[]'::jsonb),'media_ids','[]'::jsonb);
 PERFORM app.cms_apply(t,s,(d->>'draft_id')::uuid,'user',a,2,'delete_page',jsonb_build_object('id',page_id,'locale','en'));
 BEGIN PERFORM app.cms_apply(t,s,(d->>'draft_id')::uuid,'user',a,3,'update_page',page_body); RAISE EXCEPTION 'CMS_EXPECTED_REFUSAL_UPDATE_DELETED_PAGE'; EXCEPTION WHEN no_data_found THEN NULL; END;
 -- The explicit batch save operation retains its documented upsert behavior.
 PERFORM app.cms_apply(t,s,(d->>'draft_id')::uuid,'user',a,3,'apply_operations',jsonb_build_object('operations',jsonb_build_array(jsonb_build_object('operation','save_page','payload',page_body))));
 PERFORM app.cms_apply(t,s,(d->>'draft_id')::uuid,'user',a,4,'update_page',page_body);
END $aliases$;
ROLLBACK;
SELECT 'PASS: CMS_GATE controlled drafts/CAS/actor/schema/freeze/history/restore ABA/aliases' AS result;
