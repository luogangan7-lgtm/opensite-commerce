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
  'draft_pages','idempotency_records'];
 append_tables text[] := ARRAY[
  'product_content_revisions','company_profile_revisions','page_revisions',
  'blog_post_revisions','site_release_routes','site_release_templates',
  'site_release_media','draft_operations','audit_events'];
 special_tables text[] := ARRAY[
  'tenants','users','auth_sessions','api_keys','tenant_memberships','sites',
  'site_domains','media_uploads','media_blobs','media_assets','site_releases','outbox_events','webhook_inbox'];
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
     WHERE n.nspname='app' AND c.relkind IN ('r','p')) <> 41 THEN
  RAISE EXCEPTION 'role matrix requires exactly the 41 contracted app tables';
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
      IF t.relname=ANY(mutable_tables || append_tables) AND t.relname<>'idempotency_records' THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       allowed_columns := CASE t.relname
        WHEN 'idempotency_records' THEN ARRAY['tenant_id','actor_kind','actor_id','operation','idempotency_key','request_hash','expires_at','claim_token','created_at']
        WHEN 'outbox_events' THEN ARRAY['tenant_id','id','event_type','payload','delivery_target','dedup_key','created_at']
        WHEN 'webhook_inbox' THEN ARRAY['tenant_id','id','provider','provider_account_id','environment','provider_event_id','payload_hash','payload_reference','created_at']
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
        WHEN 'webhook_inbox' THEN ARRAY['status','attempts','next_attempt_at','processed_at']
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
SELECT 'PASS: three internal roles; exact 41-table and every-column privilege matrix' AS result;

-- Service capabilities are disjoint; publisher's only authority is two routines.
DO $services$
DECLARE role_name text; r record; t record; a record; privilege text;
 expected_table boolean; expected_column boolean; allowed text[]; f record;
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_domain_verifier','app_media_service','app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor'] LOOP
  SELECT * INTO r FROM pg_roles WHERE rolname=role_name;
  IF NOT FOUND THEN RAISE EXCEPTION 'missing service role: %',role_name; END IF;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole
     OR r.rolreplication OR r.rolinherit OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid)
     OR (role_name IN ('app_publish_executor','app_reliability_executor') AND EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid))
     OR has_schema_privilege(role_name,'app','CREATE')
     OR pg_has_role('app_runtime',r.oid,'SET') THEN
   RAISE EXCEPTION 'unsafe service capability: %',role_name;
  END IF;
  FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='app' AND c.relkind='r' LOOP
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    expected_table := false; allowed := ARRAY[]::text[];
    IF role_name='app_domain_verifier' AND t.relname='site_domains' AND privilege=ANY(ARRAY['SELECT','INSERT','UPDATE'])
    OR role_name='app_media_service' AND t.relname=ANY(ARRAY['media_uploads','media_blobs','media_assets']) AND privilege=ANY(ARRAY['SELECT','INSERT','UPDATE'])
    OR role_name='app_publish_executor' AND t.relname=ANY(ARRAY['sites','site_releases','site_release_routes','site_release_templates','site_release_media','media_assets','media_blobs']) AND privilege='SELECT'
    OR role_name='app_reliability_executor' AND t.relname='outbox_events' AND privilege='SELECT' THEN
     expected_table := true;
     SELECT array_agg(attname::text) INTO allowed FROM pg_attribute
      WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
    ELSIF role_name='app_publish_executor' AND privilege='UPDATE' THEN
     allowed := CASE t.relname WHEN 'sites' THEN ARRAY['published_release_id']
                              WHEN 'site_releases' THEN ARRAY['sealed']
                              WHEN 'media_assets' THEN ARRAY['visibility']
                              WHEN 'media_blobs' THEN ARRAY['status'] ELSE allowed END;
    ELSIF role_name='app_reliability_executor' AND t.relname='outbox_events' AND privilege='UPDATE' THEN
     allowed:=ARRAY['status','lease_owner','lease_until','attempts','next_attempt_at','last_error','delivered_at'];
    END IF;
    IF has_table_privilege(role_name,t.oid,privilege) IS DISTINCT FROM expected_table
     OR has_table_privilege(role_name,t.oid,privilege || ' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'service table capability mismatch: % app.% %',role_name,t.relname,privilege;
    END IF;
    IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
     FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
      expected_column := a.attname::text=ANY(allowed);
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column
       OR has_column_privilege(role_name,t.oid,a.attnum,privilege || ' WITH GRANT OPTION') THEN
       RAISE EXCEPTION 'service column capability mismatch: % app.%.% %',role_name,t.relname,a.attname,privilege;
      END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.prosecdef,p.proowner,p.proconfig,p.proacl
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF f.proname NOT IN ('guard_release_children','guard_sealed_media','seal_release','publish_release',
                      'guard_inbox_identity','guard_idempotency_identity','claim_outbox','complete_outbox','fail_outbox')
   OR f.prosecdef IS DISTINCT FROM (f.proname NOT IN ('guard_inbox_identity','guard_idempotency_identity'))
   OR f.proowner IS DISTINCT FROM (CASE WHEN f.proname IN ('guard_release_children','guard_sealed_media','seal_release','publish_release') THEN 'app_publish_executor'::regrole ELSE 'app_reliability_executor'::regrole END)
   OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')
   OR has_function_privilege('app_runtime',f.oid,'EXECUTE')
   OR has_function_privilege('app_auth',f.oid,'EXECUTE')
   OR has_function_privilege('app_domain_resolver',f.oid,'EXECUTE')
   OR has_function_privilege('app_publisher',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname IN ('seal_release','publish_release'))
   OR has_function_privilege('app_outbox_worker',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname IN ('claim_outbox','complete_outbox','fail_outbox'))
   OR has_function_privilege('app_domain_verifier',f.oid,'EXECUTE')
   OR has_function_privilege('app_media_service',f.oid,'EXECUTE') THEN
   RAISE EXCEPTION 'unsafe function capability: %',f.proname;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app')<>9 THEN
  RAISE EXCEPTION 'publisher/reliability function contract incomplete';
 END IF;
END $services$;
SELECT 'PASS: isolated service matrix and SECURITY DEFINER owner/search_path/EXECUTE' AS result;
