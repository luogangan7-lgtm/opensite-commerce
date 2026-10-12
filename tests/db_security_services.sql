-- Trusted service capability positives, tenant negatives, CAS and rollback.
-- Dependencies: migration010 APIs. Called by check_pg on a disposable cluster.
-- Invariant: runtime still prepares inputs; only isolated services confer trust.
BEGIN;
DO $fixtures$
DECLARE t uuid; other_t uuid; o uuid; s uuid; r uuid; r2 uuid; b uuid; a uuid; d uuid;
BEGIN
 INSERT INTO app.tenants(name) VALUES('W2a-services') RETURNING id INTO t;
 INSERT INTO app.tenants(name) VALUES('W2a-other-services') RETURNING id INTO other_t;
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'W2a') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'W2a') RETURNING id INTO s;
 INSERT INTO app.site_domains(tenant_id,site_id,hostname)
 VALUES(t,s,uuidv7()::text || '.example.invalid') RETURNING id INTO d;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,1,'test','{}',decode(repeat('55',32),'hex')) RETURNING id INTO r;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,2,'test','{}',decode(repeat('66',32),'hex')) RETURNING id INTO r2;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes)
 VALUES(t,'local','fixture',decode(repeat('77',32),'hex'),1) RETURNING id INTO b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type)
 VALUES(t,b,'fixture','image/png') RETURNING id INTO a;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(t,s,r,a);
 PERFORM set_config('test.svc.tenant',t::text,true);
 PERFORM set_config('test.svc.other',other_t::text,true);
 PERFORM set_config('test.svc.org',o::text,true);
 PERFORM set_config('test.svc.site',s::text,true);
 PERFORM set_config('test.svc.release',r::text,true);
 PERFORM set_config('test.svc.candidate',r2::text,true);
 PERFORM set_config('test.svc.blob',b::text,true);
 PERFORM set_config('test.svc.asset',a::text,true);
 PERFORM set_config('test.svc.domain',d::text,true);
END $fixtures$;

SET LOCAL ROLE app_runtime;
DO $runtime$
DECLARE t uuid:=current_setting('test.svc.tenant')::uuid; created uuid;
BEGIN
 -- These statements match the existing sqlc input shapes, with trusted fields omitted.
 INSERT INTO app.organizations(tenant_id,display_name,legal_name)
 VALUES(t,'Runtime Org',NULL) RETURNING id INTO created;
 INSERT INTO app.sites(tenant_id,organization_id,name,default_locale)
 VALUES(t,created,'Runtime Site','en') RETURNING id INTO created;
 INSERT INTO app.site_domains(tenant_id,site_id,hostname)
 VALUES(t,created,uuidv7()::text || '.example.invalid');
 UPDATE app.sites SET name='Renamed',default_locale='en' WHERE tenant_id=t AND id=created;
 INSERT INTO app.media_uploads(tenant_id,original_filename,expires_at)
 VALUES(t,'pending',now()+interval '1 hour');
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type)
 VALUES(t,current_setting('test.svc.blob')::uuid,'private','image/png');
 UPDATE app.media_assets SET filename='renamed' WHERE id=current_setting('test.svc.asset')::uuid;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,created,1,'runtime','{}',decode(repeat('88',32),'hex'));
 BEGIN
  PERFORM app.seal_release(t,current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid);
  RAISE EXCEPTION 'runtime sealed release';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.publish_release(t,current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid,NULL);
  RAISE EXCEPTION 'runtime used publisher capability';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $runtime$;
RESET ROLE;

SET LOCAL ROLE app_domain_verifier;
DO $domain$
DECLARE changed integer;
BEGIN
 UPDATE app.site_domains SET status='verified',verified_at=now()
  WHERE id=current_setting('test.svc.domain')::uuid;
 GET DIAGNOSTICS changed=ROW_COUNT;
 IF changed<>1 THEN RAISE EXCEPTION 'trusted verifier failed'; END IF;
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
 VALUES(current_setting('test.svc.tenant')::uuid,current_setting('test.svc.site')::uuid,
  uuidv7()::text || '.example.invalid','verified',now());
 BEGIN
  INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
  VALUES(current_setting('test.svc.other')::uuid,current_setting('test.svc.site')::uuid,
   uuidv7()::text || '.example.invalid','verified',now());
  RAISE EXCEPTION 'verifier crossed tenant';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $domain$;
RESET ROLE;

SET LOCAL ROLE app_publisher;
DO $unsafe_media$
BEGIN
 BEGIN
  PERFORM app.publish_release(current_setting('test.svc.tenant')::uuid,
   current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid,NULL);
  RAISE EXCEPTION 'unprocessed/private release media accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
END $unsafe_media$;
RESET ROLE;

SET LOCAL ROLE app_media_service;
DO $media$
DECLARE changed integer;
BEGIN
 UPDATE app.media_blobs SET status='ready' WHERE id=current_setting('test.svc.blob')::uuid;
 GET DIAGNOSTICS changed=ROW_COUNT;
 IF changed<>1 THEN RAISE EXCEPTION 'trusted blob processor failed'; END IF;
 UPDATE app.media_assets SET visibility='public' WHERE id=current_setting('test.svc.asset')::uuid;
 GET DIAGNOSTICS changed=ROW_COUNT;
 IF changed<>1 THEN RAISE EXCEPTION 'trusted media publisher failed'; END IF;
 BEGIN
  INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
  VALUES(current_setting('test.svc.other')::uuid,'local','cross',decode(repeat('99',32),'hex'),1,'ready');
  RAISE EXCEPTION 'media service crossed tenant';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $media$;
RESET ROLE;

-- Both pointer and seal must roll back together; no partially sealed publication.
SAVEPOINT publication_rollback;
SET LOCAL ROLE app_publisher;
SELECT app.publish_release(current_setting('test.svc.tenant')::uuid,
 current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid,NULL);
RESET ROLE;
ROLLBACK TO SAVEPOINT publication_rollback;
DO $rolled_back$
BEGIN
 IF (SELECT sealed FROM app.site_releases WHERE id=current_setting('test.svc.release')::uuid)
 OR (SELECT published_release_id FROM app.sites WHERE id=current_setting('test.svc.site')::uuid) IS NOT NULL THEN
  RAISE EXCEPTION 'publication did not roll back atomically';
 END IF;
END $rolled_back$;
SET LOCAL ROLE app_publisher;
SELECT app.publish_release(current_setting('test.svc.tenant')::uuid,
 current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid,NULL);
DO $publish_negatives$
BEGIN
 BEGIN
  PERFORM app.publish_release(current_setting('test.svc.tenant')::uuid,
   current_setting('test.svc.site')::uuid,current_setting('test.svc.candidate')::uuid,NULL);
  RAISE EXCEPTION 'stale expected pointer accepted';
 EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN
  PERFORM app.publish_release(current_setting('test.svc.other')::uuid,
   current_setting('test.svc.site')::uuid,current_setting('test.svc.release')::uuid,NULL);
  RAISE EXCEPTION 'publisher crossed tenant';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.sites SET published_release_id=NULL;
  RAISE EXCEPTION 'publisher wrote pointer directly';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $publish_negatives$;
SELECT app.publish_release(current_setting('test.svc.tenant')::uuid,
 current_setting('test.svc.site')::uuid,current_setting('test.svc.candidate')::uuid,
 current_setting('test.svc.release')::uuid);
RESET ROLE;
DO $published$
BEGIN
 IF (SELECT published_release_id FROM app.sites WHERE id=current_setting('test.svc.site')::uuid)
 IS DISTINCT FROM current_setting('test.svc.candidate')::uuid THEN RAISE EXCEPTION 'CAS publication missing'; END IF;
 -- The runtime-created other site is deliberately still a mutable draft.
 IF EXISTS(SELECT FROM app.site_releases WHERE site_id=current_setting('test.svc.site')::uuid AND NOT sealed) THEN
  RAISE EXCEPTION 'published release not sealed';
 END IF;
 BEGIN
  INSERT INTO app.site_release_templates(tenant_id,site_id,release_id,locale,template_kind,document)
  VALUES(current_setting('test.svc.tenant')::uuid,current_setting('test.svc.site')::uuid,
   current_setting('test.svc.candidate')::uuid,'en','blog','{}');
  RAISE EXCEPTION 'owner bypassed sealed child guard';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $published$;
ROLLBACK;
SELECT 'PASS: trusted service positives, tenant denial, media gate, publish CAS/rollback and sealing' AS result;
