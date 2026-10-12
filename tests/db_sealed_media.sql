-- Sealed releases pin their media: no post-seal downgrade to private/non-ready.
-- Dependencies: migration 010 (seal/publish, guard_sealed_media). Called by check_pg.
-- Invariant: every media row referenced by a sealed release stays public+ready.
BEGIN;
DO $fixtures$
DECLARE t uuid; o uuid; s uuid; r uuid; b uuid; b2 uuid; a uuid; free_b uuid; free_a uuid;
BEGIN
 INSERT INTO app.tenants(name) VALUES('sealed-media') RETURNING id INTO t;
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'SM') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'SM') RETURNING id INTO s;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,1,'test','{}',decode(repeat('a1',32),'hex')) RETURNING id INTO r;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local','sm-1',decode(repeat('b1',32),'hex'),1,'ready') RETURNING id INTO b;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local','sm-2',decode(repeat('b2',32),'hex'),1,'ready') RETURNING id INTO b2;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility)
 VALUES(t,b,'pinned','image/png','public') RETURNING id INTO a;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local','sm-free',decode(repeat('b3',32),'hex'),1,'ready') RETURNING id INTO free_b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility)
 VALUES(t,free_b,'free','image/png','public') RETURNING id INTO free_a;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(t,s,r,a);
 PERFORM set_config('test.sm.site',s::text,true);
 PERFORM set_config('test.sm.release',r::text,true);
 PERFORM set_config('test.sm.blob',b::text,true);
 PERFORM set_config('test.sm.blob2',b2::text,true);
 PERFORM set_config('test.sm.asset',a::text,true);
 PERFORM set_config('test.sm.free_blob',free_b::text,true);
 PERFORM set_config('test.sm.free_asset',free_a::text,true);
END $fixtures$;

SET LOCAL ROLE app_publisher;
SELECT app.publish_release(current_setting('app.tenant_id')::uuid,current_setting('test.sm.site')::uuid,
 current_setting('test.sm.release')::uuid,NULL);

SET LOCAL ROLE app_media_service;
DO $downgrades$
DECLARE a uuid:=current_setting('test.sm.asset')::uuid; b uuid:=current_setting('test.sm.blob')::uuid;
BEGIN
 BEGIN
  UPDATE app.media_assets SET visibility='private' WHERE id=a;
  RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: sealed asset made private';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.media_blobs SET status='deleting' WHERE id=b;
  RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: sealed blob left ready';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.media_assets SET blob_id=current_setting('test.sm.blob2')::uuid WHERE id=a;
  RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: sealed asset content swapped';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $downgrades$;
RESET ROLE;
-- Maintenance (RLS-bypassing) caller without tenant context must fail closed.
SELECT set_config('test.sm.tenant',current_setting('app.tenant_id'),true);
SELECT set_config('app.tenant_id','',true);
DO $no_tenant$
BEGIN
 UPDATE app.media_blobs SET status='staging' WHERE id=current_setting('test.sm.blob')::uuid;
 RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: tenantless downgrade accepted';
EXCEPTION WHEN insufficient_privilege THEN NULL;
END $no_tenant$;
-- Restore tenant context, then show safe changes and unreferenced downgrades still work.
SELECT set_config('app.tenant_id',current_setting('test.sm.tenant'),true);
SET LOCAL ROLE app_media_service;
UPDATE app.media_assets SET filename='renamed-ok' WHERE id=current_setting('test.sm.asset')::uuid;
UPDATE app.media_assets SET visibility='private' WHERE id=current_setting('test.sm.free_asset')::uuid;
UPDATE app.media_blobs SET status='deleting' WHERE id=current_setting('test.sm.free_blob')::uuid;
RESET ROLE;
DO $witness$
BEGIN
 IF EXISTS (SELECT FROM app.site_release_media rm
  JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(rm.tenant_id,rm.site_id,rm.release_id)
  JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
  JOIN app.media_blobs b ON (b.tenant_id,b.id)=(a.tenant_id,a.blob_id)
  WHERE r.sealed AND (a.visibility<>'public' OR b.status<>'ready')) THEN
  RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: sealed release references unsafe media';
 END IF;
END $witness$;
SELECT 'PASS: sealed release media pinned; safe and unreferenced media changes allowed' AS result;
ROLLBACK;

-- Downgrades of possibly-pinned media need READ COMMITTED (fresh snapshot after locks).
BEGIN ISOLATION LEVEL REPEATABLE READ;
DO $rr$
DECLARE t uuid; b uuid;
BEGIN
 INSERT INTO app.tenants(name) VALUES('sealed-media-rr') RETURNING id INTO t;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local','sm-rr',decode(repeat('c1',32),'hex'),1,'ready') RETURNING id INTO b;
 PERFORM set_config('app.tenant_id',t::text,true);
 PERFORM set_config('test.sm.rr_blob',b::text,true);
END $rr$;
SET LOCAL ROLE app_media_service;
DO $rr_denied$
BEGIN
 UPDATE app.media_blobs SET status='deleting' WHERE id=current_setting('test.sm.rr_blob')::uuid;
 RAISE EXCEPTION 'SEALED_MEDIA_TEST_FAILED: RR downgrade accepted';
EXCEPTION WHEN invalid_transaction_state THEN NULL;
END $rr_denied$;
SELECT 'PASS: media downgrade under REPEATABLE READ rejected (25001)' AS result;
ROLLBACK;
