-- 010 / Forward-only authority boundaries; depends on 001-009, called by migrate.
-- Runtime prepares pending/private inputs; isolated service roles own trust state.
-- Publish functions own the only application pointer write and lock release rows.
-- Only verifier/media/publisher may have private service login members; the
-- publish executor is an unreachable function owner, never a login capability.
DO $roles$
DECLARE role_name text; inherited record;
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver',
  'app_domain_verifier','app_media_service','app_publisher','app_publish_executor'] LOOP
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('CREATE ROLE %I NOLOGIN',role_name);
  END IF;
  EXECUTE format('ALTER ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT',role_name);
  -- Remove outward role membership, including SET ROLE paths despite NOINHERIT.
  FOR inherited IN SELECT DISTINCT parent.rolname FROM pg_auth_members m
   JOIN pg_roles parent ON parent.oid=m.roleid
   JOIN pg_roles child ON child.oid=m.member WHERE child.rolname=role_name LOOP
   EXECUTE format('REVOKE %I FROM %I CASCADE',inherited.rolname,role_name);
  END LOOP;
 END LOOP;
END $roles$;
DO $executor_members$
DECLARE member_name text;
BEGIN
 FOR member_name IN SELECT r.rolname FROM pg_auth_members m
  JOIN pg_roles r ON r.oid=m.member WHERE m.roleid='app_publish_executor'::regrole LOOP
  EXECUTE format('REVOKE app_publish_executor FROM %I CASCADE',member_name);
 END LOOP;
END $executor_members$;
GRANT USAGE ON SCHEMA app TO app_domain_verifier,app_media_service,app_publisher,app_publish_executor;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_domain_verifier,app_media_service,app_publisher,app_publish_executor;

-- Table-level grants override column revocations: remove both before narrowing.
DO $runtime_columns$
DECLARE table_name text; columns text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['tenant_memberships','site_domains','sites',
  'media_uploads','media_blobs','media_assets','site_releases'] LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO columns
   FROM pg_attribute WHERE attrelid=format('app.%I',table_name)::regclass
    AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE INSERT,UPDATE ON app.%I FROM app_runtime,PUBLIC',table_name);
  EXECUTE format('REVOKE INSERT (%s),UPDATE (%s) ON app.%I FROM app_runtime,PUBLIC',columns,columns,table_name);
 END LOOP;
END $runtime_columns$;
GRANT INSERT(tenant_id,site_id,id,hostname,created_at) ON app.site_domains TO app_runtime;
GRANT INSERT(tenant_id,id,organization_id,name,default_locale,created_at) ON app.sites TO app_runtime;
GRANT UPDATE(name,default_locale) ON app.sites TO app_runtime;
GRANT INSERT(tenant_id,id,actor_id,original_filename,declared_mime,expected_size,
 expected_sha256,expires_at,created_at) ON app.media_uploads TO app_runtime;
GRANT INSERT(tenant_id,id,blob_id,filename,mime_type,width,height,created_at) ON app.media_assets TO app_runtime;
GRANT UPDATE(filename,mime_type,width,height) ON app.media_assets TO app_runtime;

GRANT SELECT,INSERT,UPDATE ON app.site_domains TO app_domain_verifier;
CREATE POLICY domain_verifier_scope ON app.site_domains TO app_domain_verifier
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT,INSERT,UPDATE ON app.media_uploads,app.media_blobs,app.media_assets TO app_media_service;
DO $media_policies$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['media_uploads','media_blobs','media_assets'] LOOP
  EXECUTE format('CREATE POLICY media_service_scope ON app.%I TO app_media_service USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',table_name);
 END LOOP;
END $media_policies$;

ALTER TABLE app.site_releases ADD COLUMN sealed boolean NOT NULL DEFAULT false;
-- Already published releases are sealed before any application child writes.
-- 001-009 allowed publishing unsafe media; refuse to seal it silently. The
-- operator must make that media public+ready (or unpublish) and rerun 010.
LOCK TABLE app.media_assets,app.media_blobs,app.site_release_media IN SHARE MODE;
DO $published_media$
BEGIN
 IF EXISTS (SELECT FROM app.sites s
  JOIN app.site_release_media rm
   ON (rm.tenant_id,rm.site_id,rm.release_id)=(s.tenant_id,s.id,s.published_release_id)
  JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
  JOIN app.media_blobs b ON (b.tenant_id,b.id)=(a.tenant_id,a.blob_id)
  WHERE a.visibility<>'public' OR b.status<>'ready') THEN
  RAISE EXCEPTION 'PUBLISHED_RELEASE_MEDIA_NOT_PUBLIC_READY' USING ERRCODE='23514',
   HINT='Make media of published releases public+ready, or unpublish them, then rerun migration 010.';
 END IF;
END $published_media$;
UPDATE app.site_releases r SET sealed=true FROM app.sites s
 WHERE s.tenant_id=r.tenant_id AND s.id=r.site_id AND s.published_release_id=r.id;
GRANT INSERT(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash,
 restored_from,created_at) ON app.site_releases TO app_runtime;
GRANT SELECT ON app.sites,app.site_releases,app.site_release_routes,
 app.site_release_templates,app.site_release_media,app.media_assets,app.media_blobs TO app_publish_executor;
GRANT UPDATE(published_release_id) ON app.sites TO app_publish_executor;
GRANT UPDATE(sealed) ON app.site_releases TO app_publish_executor;
-- Row locks (FOR SHARE) need an UPDATE column privilege; no executor function
-- writes these columns and the executor has no login or members.
GRANT UPDATE(visibility) ON app.media_assets TO app_publish_executor;
GRANT UPDATE(status) ON app.media_blobs TO app_publish_executor;
DO $publish_policies$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['sites','site_releases','site_release_routes',
  'site_release_templates','site_release_media','media_assets','media_blobs'] LOOP
  EXECUTE format('CREATE POLICY publish_executor_scope ON app.%I TO app_publish_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',table_name);
 END LOOP;
END $publish_policies$;

-- Trigger dependency: parent release. SHARE conflicts with the sealer's UPDATE
-- lock; after waiting, PostgreSQL observes the committed sealed state. Check both
-- OLD and NEW keys so moving a child cannot mutate a sealed source release.
CREATE FUNCTION app.guard_release_children() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $guard$
DECLARE is_sealed boolean;
BEGIN
 IF TG_OP<>'INSERT' THEN
  SELECT sealed INTO is_sealed FROM app.site_releases
   WHERE tenant_id=OLD.tenant_id AND site_id=OLD.site_id AND id=OLD.release_id FOR SHARE;
  IF NOT FOUND THEN RAISE EXCEPTION 'RELEASE_NOT_VISIBLE' USING ERRCODE='23503'; END IF;
  IF is_sealed THEN RAISE EXCEPTION 'RELEASE_SEALED' USING ERRCODE='55000'; END IF;
 END IF;
 IF TG_OP<>'DELETE' THEN
  SELECT sealed INTO is_sealed FROM app.site_releases
   WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.release_id FOR SHARE;
  IF NOT FOUND THEN RAISE EXCEPTION 'RELEASE_NOT_VISIBLE' USING ERRCODE='23503'; END IF;
  IF is_sealed THEN RAISE EXCEPTION 'RELEASE_SEALED' USING ERRCODE='55000'; END IF;
 END IF;
 -- Fail closed even for a maintenance caller missing tenant context.
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $guard$;
ALTER FUNCTION app.guard_release_children() OWNER TO app_publish_executor;
REVOKE ALL ON FUNCTION app.guard_release_children() FROM PUBLIC;
CREATE TRIGGER release_routes_sealed BEFORE INSERT OR UPDATE OR DELETE ON app.site_release_routes
 FOR EACH ROW EXECUTE FUNCTION app.guard_release_children();
CREATE TRIGGER release_templates_sealed BEFORE INSERT OR UPDATE OR DELETE ON app.site_release_templates
 FOR EACH ROW EXECUTE FUNCTION app.guard_release_children();
CREATE TRIGGER release_media_sealed BEFORE INSERT OR UPDATE OR DELETE ON app.site_release_media
 FOR EACH ROW EXECUTE FUNCTION app.guard_release_children();

-- Called only by a trusted publisher, within its tenant transaction. The role is
-- not a runtime membership; application actor/scopes remain the service's duty.
CREATE FUNCTION app.seal_release(p_tenant uuid,p_site uuid,p_release uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $seal$
DECLARE is_sealed boolean;
BEGIN
 -- SHARE locks protect append/seal ordering, but do not version the parent.
 -- An old RR snapshot could miss children committed before the sealer's lock.
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'PUBLISH_READ_COMMITTED_REQUIRED' USING ERRCODE='25001';
 END IF;
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'PUBLISH_TENANT_REQUIRED' USING ERRCODE='42501';
 END IF;
 SELECT sealed INTO is_sealed FROM app.site_releases
  WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_release FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'RELEASE_NOT_VISIBLE' USING ERRCODE='23503'; END IF;
 -- Lock referenced media first: a downgrade that could not see a just-committed
 -- child is still uncommitted here, so wait for it; the next statement's fresh
 -- READ COMMITTED snapshot then sees its outcome.
 PERFORM FROM app.site_release_media rm
  JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
  JOIN app.media_blobs b ON (b.tenant_id,b.id)=(a.tenant_id,a.blob_id)
  WHERE rm.tenant_id=p_tenant AND rm.site_id=p_site AND rm.release_id=p_release
  FOR SHARE OF a,b;
 IF EXISTS (SELECT FROM app.site_release_media rm
  JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
  JOIN app.media_blobs b ON (b.tenant_id,b.id)=(a.tenant_id,a.blob_id)
  WHERE rm.tenant_id=p_tenant AND rm.site_id=p_site AND rm.release_id=p_release
   AND (a.visibility<>'public' OR b.status<>'ready')) THEN
  RAISE EXCEPTION 'RELEASE_MEDIA_NOT_PUBLIC_READY' USING ERRCODE='23514';
 END IF;
 IF NOT is_sealed THEN
  UPDATE app.site_releases SET sealed=true
   WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_release;
 END IF;
END $seal$;
ALTER FUNCTION app.seal_release(uuid,uuid,uuid) OWNER TO app_publish_executor;
REVOKE ALL ON FUNCTION app.seal_release(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.seal_release(uuid,uuid,uuid) TO app_publisher;

-- Atomic CAS pointer switch and seal. Explicit expected pointer prevents stale
-- publishers overwriting a newer release; scope and composite FK remain intact.
CREATE FUNCTION app.publish_release(p_tenant uuid,p_site uuid,p_release uuid,p_expected uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $publish$
DECLARE previous uuid;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'PUBLISH_READ_COMMITTED_REQUIRED' USING ERRCODE='25001';
 END IF;
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'PUBLISH_TENANT_REQUIRED' USING ERRCODE='42501';
 END IF;
 SELECT published_release_id INTO previous FROM app.sites
  WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'SITE_NOT_VISIBLE' USING ERRCODE='23503'; END IF;
 IF previous IS DISTINCT FROM p_expected THEN
  RAISE EXCEPTION 'PUBLISH_POINTER_CHANGED' USING ERRCODE='40001';
 END IF;
 PERFORM app.seal_release(p_tenant,p_site,p_release);
 UPDATE app.sites SET published_release_id=p_release WHERE tenant_id=p_tenant AND id=p_site;
END $publish$;
ALTER FUNCTION app.publish_release(uuid,uuid,uuid,uuid) OWNER TO app_publish_executor;
REVOKE ALL ON FUNCTION app.publish_release(uuid,uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.publish_release(uuid,uuid,uuid,uuid) TO app_publisher;

-- Sealed releases pin their media: identity, content and public/ready state of
-- referenced assets/blobs cannot change. Serialization comes from row locks:
-- seal_release takes FOR SHARE on the media rows, this UPDATE holds their row
-- lock, so whichever commits second sees the other's committed state through a
-- fresh READ COMMITTED snapshot. No release lock here: it would invert seal's
-- release-then-media lock order and invite deadlocks.
CREATE FUNCTION app.guard_sealed_media() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $media$
DECLARE pinned boolean;
BEGIN
 IF TG_TABLE_NAME='media_assets' THEN
  -- Only public assets can be referenced by a sealed release.
  IF OLD.visibility<>'public' OR (NEW.tenant_id,NEW.id,NEW.blob_id,NEW.visibility,NEW.mime_type)
   IS NOT DISTINCT FROM (OLD.tenant_id,OLD.id,OLD.blob_id,OLD.visibility,OLD.mime_type) THEN
   RETURN NEW;
  END IF;
 ELSE
  IF OLD.status<>'ready' OR (NEW.tenant_id,NEW.id,NEW.backend,NEW.storage_key,NEW.sha256,NEW.size_bytes,NEW.status)
   IS NOT DISTINCT FROM (OLD.tenant_id,OLD.id,OLD.backend,OLD.storage_key,OLD.sha256,OLD.size_bytes,OLD.status) THEN
   RETURN NEW;
  END IF;
 END IF;
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'MEDIA_CHANGE_READ_COMMITTED_REQUIRED' USING ERRCODE='25001';
 END IF;
 -- Fail closed: without tenant context RLS would hide the sealed reference.
 IF OLD.tenant_id IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'MEDIA_TENANT_REQUIRED' USING ERRCODE='42501';
 END IF;
 SELECT bool_or(r.sealed) INTO pinned FROM (
  SELECT r.sealed FROM app.site_releases r
   JOIN app.site_release_media rm ON (rm.tenant_id,rm.site_id,rm.release_id)=(r.tenant_id,r.site_id,r.id)
   JOIN app.media_assets a ON (a.tenant_id,a.id)=(rm.tenant_id,rm.media_id)
  WHERE r.tenant_id=OLD.tenant_id
   AND CASE WHEN TG_TABLE_NAME='media_assets' THEN a.id=OLD.id ELSE a.blob_id=OLD.id END) r;
 IF pinned THEN
  RAISE EXCEPTION 'SEALED_RELEASE_MEDIA_PINNED' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $media$;
ALTER FUNCTION app.guard_sealed_media() OWNER TO app_publish_executor;
REVOKE ALL ON FUNCTION app.guard_sealed_media() FROM PUBLIC;
CREATE TRIGGER media_assets_sealed BEFORE UPDATE ON app.media_assets
 FOR EACH ROW EXECUTE FUNCTION app.guard_sealed_media();
CREATE TRIGGER media_blobs_sealed BEFORE UPDATE ON app.media_blobs
 FOR EACH ROW EXECUTE FUNCTION app.guard_sealed_media();
