-- Responsibility: catalog CAS data and media-service association/delete boundaries.
-- Depends on the catalog_api forward migration, located by suffix by upgrade gates.
-- Called by check_pg.sh on isolated PG18; runtime never owns READY/public transitions.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('catalog-api-a') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('catalog-api-b') RETURNING id AS tb \gset
SELECT set_config('app.tenant_id',:'ta',true);
INSERT INTO app.products(tenant_id,product_code) VALUES(:'ta','cas') RETURNING id AS p \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'tb','foreign') RETURNING id AS pb \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'ta',:'p','SKU-CAS','') RETURNING id AS v \gset
INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
VALUES(:'ta','local','catalog-api-a',sha256('catalog-api-a'::bytea),1,'ready') RETURNING id AS b \gset
INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type) VALUES(:'ta',:'b','a.png','image/png') RETURNING id AS a \gset
INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type) VALUES(:'ta',:'b','pinned.png','image/png') RETURNING id AS ap \gset
INSERT INTO app.media_uploads(tenant_id,original_filename,expected_size,status,media_id,expires_at)
VALUES(:'ta','a.png',1,'ready',:'a',now()+interval '1 hour') RETURNING id AS u \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'ta','pin') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name,default_locale) VALUES(:'ta',:'org','pin','en') RETURNING id AS s \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
VALUES(:'ta',:'s',1,'catalog-api','{}',sha256('catalog-api'::bytea)) RETURNING id AS r \gset
INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(:'ta',:'s',:'r',:'ap');
INSERT INTO app.product_media(tenant_id,product_id,media_id,role) VALUES(:'ta',:'p',:'ap','main');
SELECT set_config('c07.ta',:'ta',true),set_config('c07.tb',:'tb',true),set_config('c07.p',:'p',true),
 set_config('c07.a',:'a',true),set_config('c07.ap',:'ap',true),set_config('c07.u',:'u',true),
 set_config('c07.v',:'v',true);
CREATE FUNCTION pg_temp.c07_reject(command text,wanted text,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 BEGIN EXECUTE command;
 EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=wanted THEN RETURN; END IF;
  RAISE EXCEPTION 'C07_WRONG_REFUSAL % expected %, got % (%)',label,wanted,SQLSTATE,SQLERRM;
 END;
 RAISE EXCEPTION 'C07_EXPECTED_REFUSAL % (%)',label,wanted;
END $$;
DO $guards$ DECLARE t uuid:=current_setting('c07.ta')::uuid; b uuid:=current_setting('c07.tb')::uuid;
 p uuid:=current_setting('c07.p')::uuid; a uuid:=current_setting('c07.a')::uuid; BEGIN
 IF (SELECT version FROM app.products WHERE tenant_id=t AND id=p)<>1
 OR (SELECT version FROM app.product_variants WHERE tenant_id=t AND id=current_setting('c07.v')::uuid)<>1
 THEN RAISE EXCEPTION 'C07_DEFAULT_VERSION'; END IF;
 PERFORM pg_temp.c07_reject(format('UPDATE app.products SET version=0 WHERE tenant_id=%L AND id=%L',t,p),'23514','product_version_positive');
 PERFORM pg_temp.c07_reject(format('UPDATE app.product_variants SET version=-1 WHERE tenant_id=%L AND id=%L',t,current_setting('c07.v')),'23514','sku_version_positive');
 PERFORM pg_temp.c07_reject(format('UPDATE app.product_variants SET lifecycle_status=''invalid'' WHERE tenant_id=%L',t),'23514','sku_lifecycle');
 PERFORM pg_temp.c07_reject(format('INSERT INTO app.media_uploads(tenant_id,original_filename,expected_size,media_id,expires_at) VALUES(%L,''foreign'',1,%L,now()+interval ''1 hour'')',b,a),'23503','upload_media_composite_fk');
END $guards$;
SELECT set_config('app.tenant_id',:'ta',true);
SET LOCAL ROLE app_runtime;
DO $cas$ DECLARE n integer; t uuid:=current_setting('c07.ta')::uuid; p uuid:=current_setting('c07.p')::uuid; BEGIN
 UPDATE app.products SET version=version+1,product_code='cas-new' WHERE tenant_id=t AND id=p AND version=1;
 GET DIAGNOSTICS n=ROW_COUNT; IF n<>1 THEN RAISE EXCEPTION 'C07_FIRST_CAS'; END IF;
 UPDATE app.products SET version=version+1,product_code='stale' WHERE tenant_id=t AND id=p AND version=1;
 GET DIAGNOSTICS n=ROW_COUNT; IF n<>0 THEN RAISE EXCEPTION 'C07_STALE_CAS'; END IF;
 PERFORM pg_temp.c07_reject(format('DELETE FROM app.media_assets WHERE tenant_id=%L AND id=%L',t,current_setting('c07.a')),'42501','runtime_media_delete');
 PERFORM pg_temp.c07_reject(format('UPDATE app.media_uploads SET status=''ready'' WHERE tenant_id=%L',t),'42501','runtime_ready');
END $cas$;
RESET ROLE;
SET LOCAL ROLE app_media_service;
DO $media$ DECLARE t uuid:=current_setting('c07.ta')::uuid; b uuid:=current_setting('c07.tb')::uuid;
 p uuid:=current_setting('c07.p')::uuid; a uuid:=current_setting('c07.a')::uuid; ap uuid:=current_setting('c07.ap')::uuid; BEGIN
 IF (SELECT count(*) FROM app.products WHERE tenant_id=t)<>1 OR EXISTS(SELECT FROM app.products WHERE tenant_id=b)
 THEN RAISE EXCEPTION 'C07_MEDIA_CATALOG_RLS'; END IF;
 INSERT INTO app.product_media(tenant_id,product_id,media_id,role) VALUES(t,p,a,'gallery');
 UPDATE app.product_media SET sort_order=2 WHERE tenant_id=t AND media_id=a;
 PERFORM pg_temp.c07_reject(format('INSERT INTO app.product_media(tenant_id,product_id,media_id,role) VALUES(%L,%L,%L,''gallery'')',b,p,a),'42501','media_cross_tenant_association');
 PERFORM pg_temp.c07_reject(format('UPDATE app.product_media SET tenant_id=%L WHERE tenant_id=%L',b,t),'42501','media_identity_update');
 PERFORM pg_temp.c07_reject(format('DELETE FROM app.media_assets WHERE tenant_id=%L AND id=%L',t,ap),'23503','release_media_pinning');
 IF NOT EXISTS(SELECT FROM app.product_media WHERE tenant_id=t AND media_id=ap) THEN RAISE EXCEPTION 'C07_PIN_LINK_LOST'; END IF;
 DELETE FROM app.product_media WHERE tenant_id=t AND media_id=a;
 DELETE FROM app.media_assets WHERE tenant_id=t AND id=a;
 IF (SELECT media_id FROM app.media_uploads WHERE tenant_id=t AND id=current_setting('c07.u')::uuid) IS NOT NULL
 THEN RAISE EXCEPTION 'C07_UPLOAD_DELETE_DID_NOT_CLEAR_MEDIA'; END IF;
 IF (SELECT tenant_id FROM app.media_uploads WHERE tenant_id=t AND id=current_setting('c07.u')::uuid)<>t
 THEN RAISE EXCEPTION 'C07_DELETE_CLEARED_TENANT'; END IF;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM app.products) OR EXISTS(SELECT FROM app.product_media) OR EXISTS(SELECT FROM app.media_assets)
 THEN RAISE EXCEPTION 'C07_MEDIA_UNSET_CONTEXT'; END IF;
END $media$;
RESET ROLE;
-- Deleting links by asset must not scan the tenant's links (migration 019).
DO $idx$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_index i WHERE i.indrelid='app.product_media'::regclass AND i.indisvalid
   AND (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(i.indkey) WITH ORDINALITY k(attnum,ord)
        JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum)=ARRAY['tenant_id','media_id'])
 THEN RAISE EXCEPTION 'C07_PRODUCT_MEDIA_ASSET_INDEX_MISSING'; END IF;
END $idx$;
ROLLBACK;
SELECT 'PASS: catalog CAS/version bounds; media service exact scope, composite FK and pinned deletion';
