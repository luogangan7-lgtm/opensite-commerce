-- Responsibility: audit the least-privilege contract for the four internal roles.
-- Dependencies: PostgreSQL 18 catalogs and app tables from migrations 001..009.
-- Caller: privileged psql test runner on an expendable database, after hardening.
-- Invariant: effective privileges (including PUBLIC/inheritance) match the contract
-- on every table AND column; table ACL absence alone cannot prove column denial.
\set ON_ERROR_STOP on
DO $audit$
DECLARE
 roles text[] := ARRAY['app_runtime','app_auth','app_domain_resolver','app_customer_auth'];
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
  'site_domains','media_uploads','media_blobs','media_assets','site_releases','outbox_events','webhook_inbox',
  'warehouses','stock_levels','inventory_reservations','inventory_movements','price_lists','price_entries',
  'shipping_zones','shipping_zone_destinations','shipping_profiles','shipping_rates',
  'tax_zones','tax_zone_locations','tax_categories','tax_rates','tax_settings',
  'customers','customer_credentials','customer_addresses','buyer_companies','buyer_roles','buyer_company_members',
  'ledger_accounts','ledger_transactions','ledger_entries',
  'orders','order_lines','order_adjustments'];
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
     WHERE n.nspname='app' AND c.relkind IN ('r','p')) <> cardinality(mutable_tables || append_tables || special_tables) THEN
  RAISE EXCEPTION 'role matrix requires exactly the classified app tables';
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
     IF privilege='SELECT' AND NOT t.relname=ANY(ARRAY['tenants','users','auth_sessions','api_keys','customer_credentials']) THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='INSERT' THEN
      IF t.relname=ANY(mutable_tables || append_tables) AND t.relname<>'idempotency_records' THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       allowed_columns := CASE t.relname
        WHEN 'ledger_accounts' THEN ARRAY['tenant_id','id','code','currency','account_type']
        WHEN 'tax_zones' THEN ARRAY['tenant_id','id','name']
        WHEN 'tax_zone_locations' THEN ARRAY['tenant_id','zone_id','country_code','subdivision']
        WHEN 'tax_categories' THEN ARRAY['tenant_id','code','name','is_exempt']
        WHEN 'tax_rates' THEN ARRAY['tenant_id','id','zone_id','category_code','rate_bp','valid_from','valid_until']
        WHEN 'tax_settings' THEN ARRAY['tenant_id','prices_include_tax']
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
        WHEN 'tax_zones' THEN ARRAY['name']
        WHEN 'tax_categories' THEN ARRAY['name','is_exempt']
        WHEN 'tax_rates' THEN ARRAY['rate_bp','valid_from','valid_until']
        WHEN 'tax_settings' THEN ARRAY['prices_include_tax']
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
    ELSIF role_name='app_customer_auth' THEN
     IF privilege='SELECT' AND t.relname=ANY(ARRAY['customers','customer_credentials']) THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='INSERT' THEN
      allowed_columns := CASE t.relname
       WHEN 'customers' THEN ARRAY['tenant_id','id','email','display_name']
       WHEN 'customer_credentials' THEN ARRAY['tenant_id','customer_id','password_hash']
       ELSE ARRAY[]::text[] END;
     ELSIF privilege='UPDATE' AND t.relname='customer_credentials' THEN
      allowed_columns := ARRAY['password_hash'];
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
DO $customer_capability$
DECLARE role_name text; f record;
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver',
  'app_domain_verifier','app_media_service','app_publisher','app_publish_executor',
  'app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_ledger_executor','app_orders_executor'] LOOP
  IF pg_has_role(role_name,'app_customer_auth','SET')
   OR pg_has_role('app_customer_auth',role_name,'SET') THEN
   RAISE EXCEPTION 'customer/staff/service capability path: %',role_name;
  END IF;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' LOOP
  IF has_function_privilege('app_customer_auth',f.oid,'EXECUTE')
   OR has_function_privilege('app_customer_auth',f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'customer auth can execute foreign routine: %',f.proname;
  END IF;
 END LOOP;
END $customer_capability$;
SELECT 'PASS: four internal roles; exact contracted-table and every-column privilege matrix' AS result;

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
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app'
  AND p.proname NOT IN ('inventory_context','inventory_immutable_movement','inventory_adjust',
   'inventory_reserve','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
   'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order',
   'buyer_roles_seed_for_tenant','customer_credentials_touch_updated_at',
   'guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger',
   'orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations') LOOP
  IF f.proname NOT IN ('guard_release_children','guard_sealed_media','seal_release','publish_release',
                      'guard_inbox_identity','guard_idempotency_identity','claim_outbox','complete_outbox','fail_outbox','idempotency_claim')
   OR f.prosecdef IS DISTINCT FROM (f.proname NOT IN ('guard_inbox_identity','guard_idempotency_identity','idempotency_claim'))
   OR f.proowner IS DISTINCT FROM (CASE WHEN f.proname IN ('guard_release_children','guard_sealed_media','seal_release','publish_release') THEN 'app_publish_executor'::regrole ELSE 'app_reliability_executor'::regrole END)
   OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')
   OR has_function_privilege('app_runtime',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname='idempotency_claim')
   OR has_function_privilege('app_auth',f.oid,'EXECUTE')
   OR has_function_privilege('app_domain_resolver',f.oid,'EXECUTE')
   OR has_function_privilege('app_publisher',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname IN ('seal_release','publish_release'))
   OR has_function_privilege('app_outbox_worker',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname IN ('claim_outbox','complete_outbox','fail_outbox'))
   OR has_function_privilege('app_domain_verifier',f.oid,'EXECUTE')
   OR has_function_privilege('app_media_service',f.oid,'EXECUTE') THEN
   RAISE EXCEPTION 'unsafe function capability: %',f.proname;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app'
  AND p.proname NOT IN ('inventory_context','inventory_immutable_movement','inventory_adjust',
   'inventory_reserve','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
   'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order',
   'buyer_roles_seed_for_tenant','customer_credentials_touch_updated_at',
   'guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger',
   'orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations'))<>10 THEN
  RAISE EXCEPTION 'publisher/reliability function contract incomplete';
 END IF;
END $services$;
SELECT 'PASS: isolated service matrix and SECURITY DEFINER owner/search_path/EXECUTE' AS result;

-- Inventory owns quantity mutation; runtime is read+commands, manager alone adjusts.
DO $inventory$
DECLARE role_name text; r record; t record; a record; privilege text; f record;
 expected boolean; allowed_tables text[]:=ARRAY['warehouses','stock_levels','inventory_reservations','inventory_movements'];
 public_commands text[]:=ARRAY['inventory_reserve','inventory_expire_due','inventory_release_non_order','inventory_commit_non_order'];
 names text[]:=ARRAY['inventory_context','inventory_immutable_movement','inventory_adjust',
  'inventory_reserve','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
  'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order'];
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_inventory_manager','app_inventory_executor'] LOOP
  SELECT * INTO STRICT r FROM pg_roles WHERE rolname=role_name;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole
   OR r.rolreplication OR r.rolinherit OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid)
   OR (role_name='app_inventory_executor' AND EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid))
   OR pg_has_role('app_runtime',r.oid,'SET') OR has_schema_privilege(role_name,'app','CREATE')
   OR NOT has_schema_privilege(role_name,'app','USAGE')
   OR has_schema_privilege(role_name,'app','USAGE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'unsafe inventory authority: %',role_name;
  END IF;
  FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    expected := privilege='SELECT' AND (t.relname=ANY(allowed_tables) OR (role_name='app_inventory_executor' AND t.relname='tenants'))
     OR role_name='app_inventory_executor' AND privilege='INSERT' AND t.relname=ANY(allowed_tables)
     OR role_name='app_inventory_executor' AND privilege='UPDATE' AND t.relname=ANY(ARRAY['stock_levels','inventory_reservations']);
    IF has_table_privilege(role_name,t.oid,privilege) IS DISTINCT FROM expected
     OR has_table_privilege(role_name,t.oid,privilege||' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'inventory table privilege mismatch: % app.% %',role_name,t.relname,privilege;
    END IF;
    IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
     FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege) IS DISTINCT FROM expected
       OR has_column_privilege(role_name,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
       RAISE EXCEPTION 'inventory column privilege mismatch: % app.%.% %',role_name,t.relname,a.attname,privilege;
      END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.prosecdef,p.proowner,p.proconfig,p.proacl FROM pg_proc p
  JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names) LOOP
  IF NOT f.prosecdef OR f.proowner<>'app_inventory_executor'::regrole
   OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
   RAISE EXCEPTION 'unsafe inventory function definition: %',f.proname;
  END IF;
  FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_domain_verifier',
   'app_media_service','app_publisher','app_publish_executor','app_inventory_manager','app_orders_executor','app_customer_auth','app_ledger_executor'] LOOP
   expected := role_name='app_runtime' AND f.proname=ANY(public_commands)
    OR role_name='app_inventory_manager' AND f.proname='inventory_adjust'
    OR role_name='app_orders_executor' AND f.proname IN('inventory_commit','inventory_release');
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'inventory routine authority mismatch: % %',role_name,f.proname;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND NOT p.proname=ANY(names) LOOP
  IF has_function_privilege('app_inventory_manager',f.oid,'EXECUTE')
   OR has_function_privilege('app_inventory_executor',f.oid,'EXECUTE') THEN
   RAISE EXCEPTION 'inventory role has foreign routine capability: %',f.proname;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'inventory function contract incomplete';
 END IF;
END $inventory$;
SELECT 'PASS: inventory exact owner/manager/runtime table-column-routine capabilities' AS result;

-- Customer trigger routines inherit the caller's authority, never add a capability.
DO $customer_triggers$
DECLARE f record; role_name text; owner_role record;
 names text[]:=ARRAY['buyer_roles_seed_for_tenant','customer_credentials_touch_updated_at'];
BEGIN
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'CUSTOMERS_TRIGGER_FUNCTION_CONTRACT_INCOMPLETE';
 END IF;
 FOR f IN SELECT p.oid,p.proname,p.proowner,p.prosecdef,p.proconfig,p.proacl,p.prorettype,p.pronargs,p.prokind
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname=ANY(names) LOOP
  SELECT * INTO STRICT owner_role FROM pg_roles WHERE oid=f.proowner;
  IF f.prokind<>'f' OR f.pronargs<>0 OR f.prorettype<>'trigger'::regtype
   OR f.prosecdef OR NOT(owner_role.rolsuper OR owner_role.rolbypassrls)
   OR EXISTS(SELECT FROM pg_class WHERE oid IN ('app.tenants'::regclass,
     'app.customers'::regclass,'app.customer_credentials'::regclass,'app.customer_addresses'::regclass,
     'app.buyer_companies'::regclass,'app.buyer_roles'::regclass,'app.buyer_company_members'::regclass)
     AND relowner IS DISTINCT FROM f.proowner)
   OR cardinality(f.proconfig) IS DISTINCT FROM 1
   OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
     WHERE grantee=0 AND privilege_type='EXECUTE') THEN
   RAISE EXCEPTION 'CUSTOMERS_UNSAFE_TRIGGER_FUNCTION: %',f.proname;
  END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   IF has_function_privilege(role_name,f.oid,'EXECUTE')
    OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'CUSTOMERS_TRIGGER_EXECUTE_CAPABILITY: % %',role_name,f.proname;
   END IF;
  END LOOP;
 END LOOP;
END $customer_triggers$;
SELECT 'PASS: customer trigger exact signature, privileged table owner, SECURITY INVOKER, search_path and no application EXECUTE' AS result;

-- Ledger writes are an atomic command; no application/service can become owner.
DO $ledger$
DECLARE r record; t record; a record; f record; privilege text; role_name text;
 expected_table boolean; expected_column boolean; allowed text[];
 names text[]:=ARRAY['guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_ledger_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole
  OR r.rolreplication OR r.rolinherit
  OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
  OR pg_has_role('app_runtime',r.oid,'SET')
  OR has_schema_privilege(r.oid,'app','CREATE')
  OR NOT has_schema_privilege(r.oid,'app','USAGE')
  OR has_schema_privilege(r.oid,'app','USAGE WITH GRANT OPTION') THEN
  RAISE EXCEPTION 'unsafe ledger executor role';
 END IF;
 FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_class c
  JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  IF t.relname=ANY(ARRAY['ledger_accounts','ledger_transactions','ledger_entries'])
   AND (NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN
   RAISE EXCEPTION 'ledger must FORCE RLS: %',t.relname;
  END IF;
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected_table:=privilege='SELECT' AND t.relname=ANY(ARRAY['ledger_accounts','ledger_transactions','ledger_entries']);
   allowed:=ARRAY[]::text[];
   IF expected_table THEN
    SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='INSERT' THEN
    allowed:=CASE t.relname
     WHEN 'ledger_transactions' THEN ARRAY['tenant_id','id','currency','source_type','source_id','event']
     WHEN 'ledger_entries' THEN ARRAY['tenant_id','transaction_id','line_no','account_id','currency','side','amount'] ELSE allowed END;
   ELSIF privilege='UPDATE' AND t.relname='ledger_transactions' THEN allowed:=ARRAY['status','posted_at','revision'];
   END IF;
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected_table
    OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'ledger executor table capability: % %',t.relname,privilege;
   END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     expected_column:=a.attname::text=ANY(allowed);
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column
      OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
      RAISE EXCEPTION 'ledger executor column capability: %.% %',t.relname,a.attname,privilege;
     END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.prosecdef,p.proowner,p.proconfig,p.proacl FROM pg_proc p
  JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF f.proname=ANY(names) THEN
   IF f.proowner<>r.oid OR f.prosecdef IS DISTINCT FROM (f.proname=ANY(ARRAY['guard_ledger_entry','assert_ledger_balance','post_ledger']))
    OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
    OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
    RAISE EXCEPTION 'unsafe ledger routine definition: %',f.proname;
   END IF;
   FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_domain_verifier','app_media_service',
    'app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_customer_auth','app_orders_executor'] LOOP
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM (role_name='app_runtime' AND f.proname='post_ledger')
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'ledger routine authority: % %',role_name,f.proname;
    END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'ledger has foreign routine capability: %',f.proname;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>5
 OR NOT EXISTS(SELECT FROM pg_proc WHERE oid='app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[])'::regprocedure AND prorettype='uuid'::regtype) THEN
  RAISE EXCEPTION 'ledger routine contract incomplete';
 END IF;
END $ledger$;
SELECT 'PASS: ledger exact executor/runtime table-column-function capabilities and FORCE RLS' AS result;

-- Orders owns sealed snapshots. Its capability only locks reservations, terminates
-- them through inventory commands and appends reserved-namespace outbox events.
DO $orders$
DECLARE r record; t record; a record; f record; role_name text; privilege text;
 expected boolean; allowed text[];
 names text[]:=ARRAY['orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_orders_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
  OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
  OR pg_has_role('app_runtime',r.oid,'SET') OR has_schema_privilege(r.oid,'app','CREATE')
  OR NOT has_schema_privilege(r.oid,'app','USAGE') OR has_schema_privilege(r.oid,'app','USAGE WITH GRANT OPTION') THEN
  RAISE EXCEPTION 'unsafe orders executor role';
 END IF;
 FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  IF t.relname IN('orders','order_lines','order_adjustments') AND (NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN
   RAISE EXCEPTION 'orders RLS must ENABLE and FORCE';
  END IF;
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=privilege='SELECT' AND t.relname IN('orders','order_lines','order_adjustments','tenants','inventory_reservations','outbox_events')
    OR privilege='INSERT' AND t.relname IN('orders','order_lines','order_adjustments');
   allowed:=ARRAY[]::text[];
   IF expected THEN SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='UPDATE' AND t.relname='orders' THEN allowed:=ARRAY['state','version','sealed','refunded_minor','updated_at'];
   ELSIF privilege='UPDATE' AND t.relname='inventory_reservations' THEN allowed:=ARRAY['id'];
   ELSIF privilege='INSERT' AND t.relname='outbox_events' THEN allowed:=ARRAY['tenant_id','id','event_type','payload','delivery_target','dedup_key','created_at'];
   END IF;
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected
    OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'orders executor table privilege mismatch app.% %',t.relname,privilege;
   END IF;
   IF privilege IN('SELECT','INSERT','UPDATE','REFERENCES') THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM (a.attname::text=ANY(allowed))
      OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
      RAISE EXCEPTION 'orders executor column privilege mismatch app.%.% %',t.relname,a.attname,privilege;
     END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.prosecdef,p.proowner,p.proconfig,p.proacl FROM pg_proc p
  JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF f.proname=ANY(names) AND (f.proowner<>'app_orders_executor'::regrole
   OR f.prosecdef IS DISTINCT FROM (f.proname<>'orders_guard')
   OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')) THEN
   RAISE EXCEPTION 'unsafe orders function %',f.proname;
  END IF;
  IF has_function_privilege('app_orders_executor',f.oid,'EXECUTE') IS DISTINCT FROM
   (f.proname=ANY(names) OR f.proname IN('inventory_commit','inventory_release'))
   OR has_function_privilege('app_orders_executor',f.oid,'EXECUTE WITH GRANT OPTION') THEN
   -- An owner implicitly has grant option for its own routines.
   IF NOT (f.proname=ANY(names) AND f.proowner='app_orders_executor'::regrole) THEN
    RAISE EXCEPTION 'orders foreign routine authority %',f.proname;
   END IF;
  END IF;
  IF f.proname=ANY(names) THEN
   FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_domain_verifier','app_media_service',
    'app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_customer_auth','app_ledger_executor'] LOOP
    expected:=role_name='app_runtime' AND f.proname IN('orders_create','orders_transition','orders_lock_reservations');
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'orders routine authority % %',role_name,f.proname;
    END IF;
   END LOOP;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'orders function contract incomplete';
 END IF;
END $orders$;
SELECT 'PASS: orders exact role/table/column/routine and FORCE RLS matrix' AS result;
