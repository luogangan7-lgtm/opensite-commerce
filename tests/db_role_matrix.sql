-- Responsibility: audit the least-privilege contract for the three base roles.
-- Dependencies: PostgreSQL 18 catalogs and app tables from migrations 001..009.
-- Caller: privileged psql test runner on an expendable database, after hardening.
-- Invariant: effective privileges (including PUBLIC/inheritance) match the contract
-- on every table AND column; table ACL absence alone cannot prove column denial.
\set ON_ERROR_STOP on
DO $audit$
DECLARE
 roles text[] := ARRAY['app_runtime','app_auth','app_domain_resolver'];
 -- Named business boundaries, independent of the migration's grant generator.
 mutable_tables text[] := ARRAY[
  'organizations','site_locales','products','product_options','product_option_values',
  'product_variants','variant_option_values','attribute_definitions',
  'product_attribute_values','product_listings','product_media','company_contacts',
  'pages','blog_posts','blog_post_publications','inquiries','site_drafts',
  'draft_pages','idempotency_records','outbox_events'];
 append_tables text[] := ARRAY[
  'product_content_revisions','company_profile_revisions','page_revisions',
  'blog_post_revisions','site_release_routes','site_release_templates',
  'site_release_media','draft_operations','audit_events'];
 special_tables text[] := ARRAY[
  'tenants','users','auth_sessions','api_keys','tenant_memberships','sites',
  'site_domains','media_uploads','media_blobs','media_assets','site_releases'];
 role_name text;
 r record;
 t record;
 a record;
 privilege text;
 all_columns text[];
 allowed_columns text[];
 expected_table boolean;
 expected_column boolean;
BEGIN
 IF (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
     WHERE n.nspname='app' AND c.relkind IN ('r','p')) <> 40 THEN
  RAISE EXCEPTION 'role matrix requires exactly the 40 contracted app tables';
 END IF;
 FOREACH role_name IN ARRAY roles LOOP
  SELECT * INTO r FROM pg_roles WHERE rolname=role_name;
  IF NOT FOUND THEN RAISE EXCEPTION 'missing internal role: %',role_name; END IF;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb
     OR r.rolcreaterole OR r.rolreplication OR r.rolinherit THEN
   RAISE EXCEPTION 'unsafe internal role attributes: % (requires NOLOGIN/NOSUPERUSER/NOBYPASSRLS/NOCREATEDB/NOCREATEROLE/NOREPLICATION/NOINHERIT)',role_name;
  END IF;
  -- Login identities may be members OF these roles. The internal roles must
  -- themselves belong to no other role, even with inheritance disabled.
  IF EXISTS (SELECT 1 FROM pg_auth_members WHERE member=r.oid) THEN
   RAISE EXCEPTION 'internal role has unexpected membership: %',role_name;
  END IF;
  IF NOT has_schema_privilege(role_name,'app','USAGE')
     OR has_schema_privilege(role_name,'app','CREATE')
     OR has_schema_privilege(role_name,'app','USAGE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'incorrect app schema privileges: %',role_name;
  END IF;
  FOR t IN
   SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='app' AND c.relkind IN ('r','p') ORDER BY c.relname
  LOOP
   IF NOT t.relname=ANY(mutable_tables || append_tables || special_tables) THEN
    RAISE EXCEPTION 'unclassified app table: %',t.relname;
   END IF;
   SELECT array_agg(attname::text ORDER BY attnum) INTO all_columns
    FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE',
                                    'TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    allowed_columns := ARRAY[]::text[];
    expected_table := false;
    IF role_name='app_runtime' THEN
     IF privilege='SELECT' AND NOT t.relname=ANY(ARRAY['tenants','users','auth_sessions','api_keys']) THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='INSERT' THEN
      IF t.relname=ANY(mutable_tables || append_tables) THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       allowed_columns := CASE t.relname
        WHEN 'site_domains' THEN ARRAY['tenant_id','site_id','id','hostname','created_at']
        WHEN 'sites' THEN array_remove(all_columns,'published_release_id')
        WHEN 'media_uploads' THEN ARRAY['tenant_id','id','actor_id','original_filename',
         'declared_mime','expected_size','expected_sha256','expires_at','created_at']
        WHEN 'media_assets' THEN array_remove(all_columns,'visibility')
        WHEN 'site_releases' THEN ARRAY['tenant_id','site_id','id','release_no',
         'renderer_build_id','manifest','manifest_hash','restored_from','created_at']
        ELSE ARRAY[]::text[] END;
      END IF;
     ELSIF privilege='UPDATE' THEN
      IF t.relname=ANY(mutable_tables) THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       allowed_columns := CASE t.relname
        WHEN 'sites' THEN ARRAY['name','default_locale']
        WHEN 'media_assets' THEN ARRAY['filename','mime_type','width','height']
        ELSE ARRAY[]::text[] END;
      END IF;
     END IF;
    ELSIF role_name='app_auth' THEN
     IF (privilege='SELECT' AND t.relname=ANY(ARRAY['users','auth_sessions','api_keys','tenant_memberships']))
        OR (privilege='INSERT' AND t.relname='auth_sessions') THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='UPDATE' AND t.relname='auth_sessions' THEN
      allowed_columns := ARRAY['revoked_at'];
     END IF;
    ELSIF role_name='app_domain_resolver' AND privilege='SELECT' THEN
     allowed_columns := CASE t.relname
      WHEN 'site_domains' THEN ARRAY['tenant_id','site_id','normalized_hostname','status','verified_at']
      WHEN 'sites' THEN ARRAY['tenant_id','id','published_release_id','default_locale']
      ELSE ARRAY[]::text[] END;
    END IF;
    IF has_table_privilege(role_name,t.oid,privilege) IS DISTINCT FROM expected_table THEN
     RAISE EXCEPTION 'table privilege mismatch: % app.% % expected %',
      role_name,t.relname,privilege,expected_table;
    END IF;
    IF has_table_privilege(role_name,t.oid,privilege || ' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'unexpected table GRANT OPTION: % app.% %',role_name,t.relname,privilege;
    END IF;
    -- Only these privileges can also be conferred by column ACLs.
    IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
     FOR a IN SELECT attnum,attname FROM pg_attribute
      WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped ORDER BY attnum
     LOOP
      expected_column := a.attname::text=ANY(allowed_columns);
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column THEN
       RAISE EXCEPTION 'column privilege mismatch: % app.%.% % expected %',
        role_name,t.relname,a.attname,privilege,expected_column;
      END IF;
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege || ' WITH GRANT OPTION') THEN
       RAISE EXCEPTION 'unexpected column GRANT OPTION: % app.%.% %',role_name,t.relname,a.attname,privilege;
      END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
 END LOOP;
END $audit$;
SELECT 'PASS: three internal roles; exact 40-table and every-column privilege matrix' AS result;
