-- Runtime mutation boundaries; owned fixtures roll back. Requires 001-010.
-- check_pg runs all cases; -v attack=<name> isolates each pre-010 red witness.
-- Only the documented SQLSTATE counts as denial; FK/parser failures never pass.
\if :{?attack}
\else
\set attack all
\endif
BEGIN;
SELECT set_config('test.w2a.attack', :'attack', true);
DO $fixtures$
DECLARE t uuid; u uuid; o uuid; s uuid; r uuid; r2 uuid; p uuid; b uuid; a uuid; d uuid; upload uuid;
BEGIN
 INSERT INTO app.tenants(name) VALUES ('W2a-security') RETURNING id INTO t;
 INSERT INTO app.users(email,password_hash)
 VALUES (uuidv7()::text || '@example.invalid','test-not-a-password') RETURNING id INTO u;
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'viewer');
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'W2a') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'W2a') RETURNING id INTO s;
 INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
 VALUES(t,s,uuidv7()::text || '.example.invalid','verified',now()) RETURNING id INTO d;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,1,'test','{}',decode(repeat('11',32),'hex')) RETURNING id INTO r;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,s,2,'test','{}',decode(repeat('22',32),'hex')) RETURNING id INTO r2;
 INSERT INTO app.pages(tenant_id,site_id,page_key) VALUES(t,s,'home') RETURNING id INTO p;
 INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document)
 VALUES(t,s,p,'en',1,'{}');
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,'local','fixture',decode(repeat('33',32),'hex'),1,'staging') RETURNING id INTO b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type)
 VALUES(t,b,'fixture','image/png') RETURNING id INTO a;
 INSERT INTO app.media_uploads(tenant_id,original_filename,expires_at)
 VALUES(t,'fixture',now()+interval '1 hour') RETURNING id INTO upload;
 UPDATE app.sites SET published_release_id=r WHERE tenant_id=t AND id=s;
 -- Pre-010 has no sealed field: a published release is still an append target.
 -- Post-010 this fixture explicitly represents an already sealed release.
 IF EXISTS (SELECT FROM pg_attribute WHERE attrelid='app.site_releases'::regclass
            AND attname='sealed' AND NOT attisdropped) THEN
  EXECUTE 'UPDATE app.site_releases SET sealed=true WHERE tenant_id=$1 AND id=$2' USING t,r;
 END IF;
 PERFORM set_config('app.tenant_id',t::text,true);
 PERFORM set_config('test.w2a.tenant',t::text,true);
 PERFORM set_config('test.w2a.user',u::text,true);
 PERFORM set_config('test.w2a.org',o::text,true);
 PERFORM set_config('test.w2a.site',s::text,true);
 PERFORM set_config('test.w2a.release',r::text,true);
 PERFORM set_config('test.w2a.candidate',r2::text,true);
 PERFORM set_config('test.w2a.page',p::text,true);
 PERFORM set_config('test.w2a.blob',b::text,true);
 PERFORM set_config('test.w2a.asset',a::text,true);
 PERFORM set_config('test.w2a.domain',d::text,true);
 PERFORM set_config('test.w2a.upload',upload::text,true);
END $fixtures$;
SET LOCAL ROLE app_runtime;
DO $cases$
DECLARE c record; actual text; ran integer := 0;
BEGIN
 FOR c IN SELECT * FROM (VALUES
 ('a_insert','42501',$sql$INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.user')::uuid,'owner')
  ON CONFLICT(tenant_id,user_id) DO UPDATE SET role='owner'$sql$),
 ('a_update','42501',$sql$UPDATE app.tenant_memberships SET role='owner'
  WHERE tenant_id=current_setting('test.w2a.tenant')::uuid$sql$),
 ('b_insert','42501',$sql$INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.site')::uuid,
  'forged.example.invalid','verified',now())$sql$),
 ('b_update','42501',$sql$UPDATE app.site_domains SET status='verified',verified_at=now()
  WHERE id=current_setting('test.w2a.domain')::uuid$sql$),
 ('b_hostname','42501',$sql$UPDATE app.site_domains SET hostname='hijacked.example.invalid'
  WHERE id=current_setting('test.w2a.domain')::uuid$sql$),
 ('c_upload_insert','42501',$sql$INSERT INTO app.media_uploads(tenant_id,original_filename,expires_at,status)
  VALUES(current_setting('test.w2a.tenant')::uuid,'forged',now()+interval '1 hour','ready')$sql$),
 ('c_upload_update','42501',$sql$UPDATE app.media_uploads SET status='ready'
  WHERE id=current_setting('test.w2a.upload')::uuid$sql$),
 ('c_blob_insert','42501',$sql$INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
  VALUES(current_setting('test.w2a.tenant')::uuid,'local','forged',decode(repeat('44',32),'hex'),1,'ready')$sql$),
 ('c_blob_update','42501',$sql$UPDATE app.media_blobs SET status='ready'
  WHERE id=current_setting('test.w2a.blob')::uuid$sql$),
 ('c_asset_insert','42501',$sql$INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.blob')::uuid,
  'forged','image/png','public')$sql$),
 ('c_asset_update','42501',$sql$UPDATE app.media_assets SET visibility='public'
  WHERE id=current_setting('test.w2a.asset')::uuid$sql$),
 ('d_insert','42501',$sql$INSERT INTO app.sites(tenant_id,id,organization_id,name,published_release_id)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.site')::uuid,
  current_setting('test.w2a.org')::uuid,'forged',current_setting('test.w2a.candidate')::uuid)
  ON CONFLICT(tenant_id,id) DO UPDATE SET published_release_id=EXCLUDED.published_release_id$sql$),
 ('d_update','42501',$sql$UPDATE app.sites SET published_release_id=current_setting('test.w2a.candidate')::uuid
  WHERE id=current_setting('test.w2a.site')::uuid$sql$),
 ('e_routes','55000',$sql$INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.site')::uuid,
  current_setting('test.w2a.release')::uuid,'en','/late',current_setting('test.w2a.page')::uuid,1)$sql$),
 ('e_templates','55000',$sql$INSERT INTO app.site_release_templates(tenant_id,site_id,release_id,locale,template_kind,document)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.site')::uuid,
  current_setting('test.w2a.release')::uuid,'en','product','{}')$sql$),
 ('e_media','55000',$sql$INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id)
  VALUES(current_setting('test.w2a.tenant')::uuid,current_setting('test.w2a.site')::uuid,
  current_setting('test.w2a.release')::uuid,current_setting('test.w2a.asset')::uuid)$sql$)
 ) AS attacks(name,expected_sqlstate,command)
 LOOP
  IF current_setting('test.w2a.attack') NOT IN ('all',c.name) THEN CONTINUE; END IF;
  ran := ran+1;
  BEGIN
   EXECUTE c.command;
   RAISE EXCEPTION 'W2A_PROTECTION_MISSING:%',c.name;
  EXCEPTION WHEN OTHERS THEN
   GET STACKED DIAGNOSTICS actual=RETURNED_SQLSTATE;
   IF actual<>c.expected_sqlstate THEN RAISE; END IF;
  END;
  RAISE NOTICE 'PASS W2A attack=% expected_sqlstate=%',c.name,c.expected_sqlstate;
 END LOOP;
 IF ran=0 THEN RAISE EXCEPTION 'unknown W2a attack'; END IF;
END $cases$;
ROLLBACK;
