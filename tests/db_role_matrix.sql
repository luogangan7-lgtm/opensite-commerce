-- Responsibility: audit effective identity, business, queue and ops capability boundaries.
-- Dependencies: PostgreSQL 18 catalogs, app/queue schema and all current forward migrations, including identity, CMS, checkout, customer auth and fulfillment.
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
  'tenant_lifecycles','tenant_lifecycle_receipts','tenant_lifecycle_events',
  'tenants','users','auth_sessions','api_keys','tenant_memberships','sites',
  'site_domains','media_uploads','media_blobs','media_assets','site_releases','outbox_events','webhook_inbox',
  'warehouses','stock_levels','inventory_reservations','inventory_movements','price_lists','price_entries',
  'shipping_zones','shipping_zone_destinations','shipping_profiles','shipping_rates',
  'tax_zones','tax_zone_locations','tax_categories','tax_rates','tax_settings',
  'customers','customer_credentials','customer_addresses','buyer_companies','buyer_roles','buyer_company_members',
  'customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits','customer_auth_global_limits',
  'ledger_accounts','ledger_transactions','ledger_entries',
  'carts','cart_lines',
  'orders','order_lines','order_adjustments','shipments','shipment_lines','returns','return_lines','notification_deliveries','notification_redrive_requests','quotes','quote_lines','quote_approvals','quote_conversions',
  'auth_account_attempts','auth_login_attempts','known_devices','draft_page_media','checkout_sessions','checkout_attempts','payment_provider_accounts','payment_events','payment_event_deliveries','payment_return_refunds','payment_refund_attempts','payment_refund_reversals','payment_refund_terminals','payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions',
  'site_release_skus','storefront_serving','serving_invalidations','data_rights_exports','data_rights_erasures','data_rights_erased_orders'];
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
     IF privilege='SELECT' AND NOT t.relname=ANY(ARRAY['tenant_lifecycle_receipts','tenant_lifecycle_events','payment_return_refunds','payment_refund_attempts','payment_refund_reversals','payment_refund_terminals','payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions','payment_provider_accounts','tenants','users','auth_sessions','api_keys','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits','customer_auth_global_limits','auth_account_attempts','auth_login_attempts','known_devices','data_rights_exports','data_rights_erasures','data_rights_erased_orders']) THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='SELECT' AND t.relname='customer_guest_order_links' THEN
      allowed_columns := ARRAY['tenant_id','customer_id','order_id'];
     ELSIF privilege='INSERT' THEN
      IF t.relname=ANY(mutable_tables || append_tables) AND t.relname NOT IN ('idempotency_records','site_drafts') THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       IF t.relname=ANY(ARRAY['checkout_sessions','checkout_attempts']) THEN expected_table := true; END IF;
       allowed_columns := CASE t.relname
        WHEN 'payment_events' THEN ARRAY['tenant_id','id','provider_account_id','provider_ref','event_key','claimed_order_id','order_version','status','amount_minor','currency','provider_refund_id']
        WHEN 'payment_event_deliveries' THEN ARRAY['tenant_id','id','payment_event_id']
        WHEN 'checkout_sessions' THEN all_columns
        WHEN 'checkout_attempts' THEN all_columns
        WHEN 'site_drafts' THEN ARRAY['tenant_id','site_id','id','base_release_id','revision','status','created_at']
        WHEN 'carts' THEN ARRAY['tenant_id','id','customer_id','guest_token_hash','currency']
        WHEN 'cart_lines' THEN ARRAY['tenant_id','cart_id','sku_id','quantity']
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
        WHEN 'sites' THEN array_remove(array_remove(all_columns,'published_release_id'),'publication_revision')
        WHEN 'media_uploads' THEN ARRAY['tenant_id','id','actor_id','original_filename',
         'declared_mime','expected_size','expected_sha256','expires_at','created_at']
        WHEN 'media_assets' THEN array_remove(all_columns,'visibility')
        WHEN 'site_releases' THEN ARRAY['tenant_id','site_id','id','release_no',
         'renderer_build_id','manifest','manifest_hash','restored_from','created_at']
        ELSE ARRAY[]::text[] END;
      END IF;
     ELSIF privilege='DELETE' AND t.relname='cart_lines' THEN
      expected_table := true;
     ELSIF privilege='UPDATE' THEN
      IF t.relname=ANY(mutable_tables) AND t.relname<>'site_drafts' THEN
       allowed_columns := all_columns;
       expected_table := true;
      ELSE
       allowed_columns := CASE t.relname
        WHEN 'checkout_sessions' THEN ARRAY['phase','provider_ref','start_result','final_result','claim_kind','claim_key','updated_at','refunded_net_minor','refunded_tax_minor','refunded_shipping_minor']
        WHEN 'payment_events' THEN ARRAY['applied','error_code']
        WHEN 'checkout_attempts' THEN ARRAY['result']
        WHEN 'site_drafts' THEN ARRAY['tenant_id','site_id','id','base_release_id','revision','status','created_at']
        WHEN 'carts' THEN ARRAY['version']
        WHEN 'cart_lines' THEN ARRAY['quantity']
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
     ELSIF t.relname=ANY(ARRAY['auth_account_attempts','auth_login_attempts','known_devices']) THEN
      IF privilege='SELECT' THEN allowed_columns:=all_columns;
      ELSIF privilege='INSERT' THEN
       allowed_columns:=CASE WHEN t.relname='known_devices' THEN ARRAY['id','user_id','cookie_hash','expires_at','created_at'] ELSE all_columns END;
      ELSIF privilege='UPDATE' THEN
       allowed_columns:=CASE WHEN t.relname='known_devices' THEN ARRAY['revoked_at','failure_count'] ELSE ARRAY['failure_count','window_started_at','cooldown_until','last_failed_at'] END;
      END IF;
     ELSIF t.relname='tenants' AND privilege='SELECT' THEN
      allowed_columns:=ARRAY['id','status'];
     ELSIF privilege='UPDATE' AND t.relname='auth_sessions' THEN
      allowed_columns := ARRAY['revoked_at','last_seen_at'];
     END IF;
    ELSIF role_name='app_customer_auth' THEN
     IF privilege='SELECT' AND t.relname=ANY(ARRAY['customers','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links']) THEN
      allowed_columns := all_columns;
      expected_table := true;
     ELSIF privilege='INSERT' THEN
      allowed_columns := CASE t.relname
       WHEN 'customers' THEN ARRAY['tenant_id','id','email','display_name']
       ELSE ARRAY[]::text[] END;
     END IF;
    ELSIF role_name='app_domain_resolver' AND privilege='SELECT' THEN
     allowed_columns := CASE t.relname
      WHEN 'site_domains' THEN ARRAY['tenant_id','site_id','normalized_hostname','status','verified_at']
      WHEN 'sites' THEN ARRAY['tenant_id','id','published_release_id','default_locale','publication_revision']
      WHEN 'site_releases' THEN ARRAY['tenant_id','site_id','id','sealed','manifest']
      -- Storefront serving (ADR_STOREFRONT_SERVING): the current serving row only.
      WHEN 'storefront_serving' THEN ARRAY['tenant_id','site_id','publication_revision','body','fallback_body','body_sha256','fallback_sha256','valid_until']
      WHEN 'serving_invalidations' THEN ARRAY['tenant_id','site_id','created_at']
      WHEN 'site_release_routes' THEN ARRAY['tenant_id','site_id','release_id','locale','path','page_id','page_revision']
      WHEN 'page_revisions' THEN ARRAY['tenant_id','site_id','page_id','locale','revision','document']
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
  'app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_ledger_executor','app_orders_executor','app_queue_worker','app_outbox_tenant_broker','app_fulfillment_executor','app_returns_service'] LOOP
  IF pg_has_role(role_name,'app_customer_auth','SET')
   OR pg_has_role('app_customer_auth',role_name,'SET') THEN
   RAISE EXCEPTION 'customer/staff/service capability path: %',role_name;
  END IF;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname NOT IN('customer_auth_context','customer_claim_guest_orders','customer_prepare_registration',
   'customer_consume_verification','customer_prepare_reset','customer_consume_reset','customer_save_session',
   'customer_session','customer_revoke_session','customer_admit','customer_failed') LOOP
  IF has_function_privilege('app_customer_auth',f.oid,'EXECUTE')
   OR has_function_privilege('app_customer_auth',f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'customer auth can execute foreign routine: %',f.proname;
  END IF;
 END LOOP;
END $customer_capability$;
SELECT 'PASS: four internal roles; exact contracted-table and every-column privilege matrix' AS result;

-- Service capabilities are disjoint; publisher's only authority is two routines.
-- Global auth is isolated by effective table/column ACL, not a fake tenant realm.
-- Bootstrap is execute-only; its unreachable owner has minimal credential DML.
DO $identity$
DECLARE role_name text; r record; t record; a record; p text; allowed text[]; f record;
 names text[]:=ARRAY['auth_record_failure','guard_known_device','auth_fail_known_device','bootstrap_admin','auth_prune_attempts','auth_lock_login_user','auth_revoke_user_sessions'];
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_credential_writer','app_admin_bootstrap'] LOOP
  SELECT * INTO STRICT r FROM pg_roles WHERE rolname=role_name;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
   OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid)
   OR (role_name='app_credential_writer' AND EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid))
   OR NOT has_schema_privilege(role_name,'app','USAGE') OR has_schema_privilege(role_name,'app','CREATE')
   OR has_schema_privilege(role_name,'app','USAGE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'unsafe identity capability role: %',role_name; END IF;
  FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN ('r','p') LOOP
   FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    allowed:=ARRAY[]::text[];
    IF role_name='app_credential_writer' THEN
     IF p='SELECT' THEN allowed:=CASE t.relname WHEN 'users' THEN ARRAY['id','password_hash','status'] WHEN 'tenants' THEN ARRAY['id','status']
      WHEN 'tenant_memberships' THEN ARRAY['tenant_id','user_id','role','status']
      WHEN 'api_keys' THEN ARRAY['tenant_id','id','issued_by','key_prefix','key_hash','scopes','expires_at','revoked_at','created_at','last_used_at']
      WHEN 'auth_sessions' THEN ARRAY['id','user_id','expires_at','revoked_at','created_at','last_seen_at']
      WHEN 'auth_account_attempts' THEN ARRAY['identifier_normalized','window_started_at','cooldown_until','last_failed_at']
      WHEN 'auth_login_attempts' THEN ARRAY['identifier_normalized','ip','window_started_at','cooldown_until','last_failed_at'] ELSE allowed END;
     ELSIF p='INSERT' THEN allowed:=CASE t.relname WHEN 'users' THEN ARRAY['email','password_hash'] WHEN 'tenants' THEN ARRAY['id','name']
      WHEN 'tenant_memberships' THEN ARRAY['tenant_id','user_id','role']
      WHEN 'buyer_roles' THEN ARRAY['tenant_id','code']
      WHEN 'api_keys' THEN ARRAY['tenant_id','id','issued_by','key_prefix','key_hash','scopes','expires_at'] ELSE allowed END;
     ELSIF p='UPDATE' THEN allowed:=CASE t.relname WHEN 'users' THEN ARRAY['password_hash']
      WHEN 'tenants' THEN ARRAY['status'] WHEN 'tenant_memberships' THEN ARRAY['status']
      WHEN 'api_keys' THEN ARRAY['revoked_at','last_used_at'] WHEN 'auth_sessions' THEN ARRAY['revoked_at'] ELSE allowed END; END IF;
    END IF;
    IF has_table_privilege(role_name,t.oid,p) IS DISTINCT FROM
     (role_name='app_credential_writer' AND p='DELETE' AND t.relname IN ('auth_account_attempts','auth_login_attempts'))
     OR has_table_privilege(role_name,t.oid,p||' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'identity role has table authority: % % %',role_name,t.relname,p; END IF;
    IF p=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
     FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
      IF has_column_privilege(role_name,t.oid,a.attnum,p) IS DISTINCT FROM (a.attname::text=ANY(allowed))
       OR has_column_privilege(role_name,t.oid,a.attnum,p||' WITH GRANT OPTION') THEN
       RAISE EXCEPTION 'identity column authority mismatch: % %.% %',role_name,t.relname,a.attname,p; END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity,c.relacl,c.relowner
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relname=ANY(ARRAY['auth_account_attempts','auth_login_attempts','known_devices']) LOOP
  IF t.relrowsecurity OR t.relforcerowsecurity OR EXISTS(SELECT FROM pg_attribute WHERE attrelid=t.oid AND attname='tenant_id' AND NOT attisdropped)
   OR EXISTS(SELECT FROM aclexplode(coalesce(t.relacl,acldefault('r',t.relowner))) WHERE grantee<>t.relowner AND NOT (t.relname IN ('auth_account_attempts','auth_login_attempts')
     AND grantee='app_credential_writer'::regrole AND privilege_type='DELETE' AND NOT is_grantable)) THEN
   RAISE EXCEPTION 'global auth table contract mismatch: %',t.relname; END IF;
  IF EXISTS(SELECT FROM pg_attribute col CROSS JOIN LATERAL aclexplode(col.attacl) acl WHERE col.attrelid=t.oid AND acl.grantee NOT IN (t.relowner,'app_auth'::regrole,'app_credential_writer'::regrole)) THEN
   RAISE EXCEPTION 'non-auth global column access: %',t.relname; END IF;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.prosecdef,p.proowner,p.proconfig,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname=ANY(names) LOOP
  IF f.proowner<>'app_credential_writer'::regrole OR f.prosecdef IS DISTINCT FROM (f.proname IN ('bootstrap_admin','auth_prune_attempts','auth_lock_login_user','auth_revoke_user_sessions'))
   OR cardinality(f.proconfig) IS DISTINCT FROM (CASE WHEN f.proname='bootstrap_admin' THEN 2 ELSE 1 END)
   OR replace(f.proconfig[1],' ','')<>'search_path=pg_catalog,app,pg_temp'
   OR (f.proname='bootstrap_admin' AND f.proconfig[2]<>'app.tenant_id=')
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE
    grantee<>f.proowner AND NOT (grantee='app_auth'::regrole AND f.proname IN ('auth_record_failure','auth_fail_known_device','auth_prune_attempts','auth_lock_login_user')
    OR grantee='app_admin_bootstrap'::regrole AND f.proname='bootstrap_admin'))
   OR f.oid<>to_regprocedure(CASE f.proname WHEN 'auth_record_failure' THEN 'app.auth_record_failure(text,inet)'
    WHEN 'guard_known_device' THEN 'app.guard_known_device()' WHEN 'auth_fail_known_device' THEN 'app.auth_fail_known_device(uuid)'
    WHEN 'bootstrap_admin' THEN 'app.bootstrap_admin(text,text,text)'
    WHEN 'auth_prune_attempts' THEN 'app.auth_prune_attempts(integer)'
    WHEN 'auth_lock_login_user' THEN 'app.auth_lock_login_user(uuid,text)'
    WHEN 'auth_revoke_user_sessions' THEN 'app.auth_revoke_user_sessions()' END) THEN RAISE EXCEPTION 'unsafe identity function %',f.proname; END IF;
  FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_domain_verifier','app_media_service','app_publisher',
   'app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_customer_auth','app_ledger_executor','app_orders_executor','app_queue_worker','app_outbox_tenant_broker','app_credential_writer','app_admin_bootstrap','app_fulfillment_executor','app_returns_service'] LOOP
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM
    (role_name='app_credential_writer' OR role_name='app_auth' AND f.proname=ANY(ARRAY['auth_record_failure','auth_fail_known_device','auth_prune_attempts','auth_lock_login_user'])
     OR role_name='app_admin_bootstrap' AND f.proname='bootstrap_admin') THEN RAISE EXCEPTION 'identity function role mismatch: % %',role_name,f.proname; END IF;
   IF role_name<>'app_credential_writer' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'identity function grant option: % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'identity function contract incomplete'; END IF;
 -- New capability roles cannot execute older business service functions either.
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND NOT p.proname=ANY(names)
  AND p.proname NOT IN ('auth_role_scopes','auth_create_api_key','auth_revoke_api_key','auth_touch_api_key','auth_revalidate_tenant',
   'tenant_lifecycle_read_fence','tenant_lifecycle_write_fence') LOOP
  IF has_function_privilege('app_admin_bootstrap',f.oid,'EXECUTE') OR has_function_privilege('app_credential_writer',f.oid,'EXECUTE') THEN
   RAISE EXCEPTION 'identity role has unrelated function authority: %',f.proname; END IF;
 END LOOP;
END $identity$;
SELECT 'PASS: global auth exact ACL; unreachable credential writer and execute-only first-owner capability' AS result;

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
    OR role_name='app_media_service' AND t.relname='products' AND privilege='SELECT'
    OR role_name='app_media_service' AND t.relname='product_media' AND privilege=ANY(ARRAY['SELECT','INSERT','DELETE'])
    OR role_name='app_media_service' AND t.relname='media_assets' AND privilege='DELETE'
    OR role_name='app_publish_executor' AND t.relname=ANY(ARRAY['sites','site_releases','site_release_routes','site_release_templates','site_release_media','media_assets','media_blobs','site_drafts','draft_pages','draft_operations','pages','page_revisions','products','product_variants','product_listings','product_content_revisions','price_lists','price_entries','draft_page_media']) AND privilege='SELECT'
    OR role_name='app_publish_executor' AND t.relname=ANY(ARRAY['draft_pages','draft_page_media','pages','page_revisions','site_release_routes','site_release_media','draft_operations','audit_events']) AND privilege='INSERT'
    OR role_name='app_publish_executor' AND t.relname=ANY(ARRAY['draft_pages','draft_page_media']) AND privilege='DELETE'
    OR role_name='app_publish_executor' AND t.relname=ANY(ARRAY['site_release_skus','storefront_serving']) AND privilege=ANY(ARRAY['SELECT','INSERT'])
    OR role_name='app_publish_executor' AND t.relname='serving_invalidations' AND privilege=ANY(ARRAY['SELECT','DELETE'])
    OR role_name='app_reliability_executor' AND t.relname='outbox_events' AND privilege='SELECT' THEN
     expected_table := true;
     SELECT array_agg(attname::text) INTO allowed FROM pg_attribute
      WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
    ELSIF role_name='app_media_service' AND t.relname='product_media' AND privilege='UPDATE' THEN
     allowed := ARRAY['sort_order'];
    ELSIF role_name='app_publish_executor' AND privilege='INSERT' THEN
     allowed:=CASE t.relname WHEN 'site_drafts' THEN ARRAY['tenant_id','site_id','id','base_release_id','revision','status','is_cms','locale','title','description','navigation'] WHEN 'site_releases' THEN ARRAY['tenant_id','site_id','id','release_no','renderer_build_id','manifest','manifest_hash'] WHEN 'serving_invalidations' THEN ARRAY['tenant_id','site_id','reason'] ELSE allowed END;
    ELSIF role_name='app_publish_executor' AND privilege='UPDATE' THEN
     allowed := CASE t.relname WHEN 'sites' THEN ARRAY['published_release_id','publication_revision']
                              WHEN 'site_drafts' THEN ARRAY['revision','status','locale','title','description','navigation']
                              WHEN 'draft_pages' THEN ARRAY['path','document']
                              WHEN 'site_releases' THEN ARRAY['sealed']
                              WHEN 'media_assets' THEN ARRAY['visibility']
                              WHEN 'media_blobs' THEN ARRAY['status']
                              WHEN 'storefront_serving' THEN ARRAY['release_id','publication_revision','serving_generation','body','fallback_body','body_sha256','fallback_sha256','valid_until','built_through_seq','built_at'] ELSE allowed END;
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
  AND NOT starts_with(p.proname,'data_rights_')
  AND NOT starts_with(p.proname,'tenant_lifecycle_')
  AND p.proname NOT LIKE 'cms_%' AND p.proname NOT IN ('catalog_listing_version','pricing_currency','pricing_write_list','pricing_write_entry','cart_tenant_active','cart_guard_version','inventory_context','inventory_immutable_movement','inventory_adjust',
   'inventory_reserve','inventory_reserve_owned','inventory_reserve_non_order','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
   'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order',
   'buyer_roles_seed_for_tenant','customer_credentials_touch_updated_at',
   'guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger','ledger_post_core',
   'payments_finalize_refund_core','payments_guard_refund_case','payments_retain_refund_case','payments_refund_case_evidence','payments_replay_refund_case','payments_resolve_refund_case','payments_open_refund_cases',
   'payments_post_refund_compensation','payments_guard_compensation_source','payments_assert_compensation_journal',
   'payments_finalize_refund','payments_refund_terminal','payments_post_return_refund','payments_guard_refund_terminal','orders_compensate_refund','returns_compensate_refund',
   'fulfillment_allocate','fulfillment_return_json','fulfillment_shipment_json','fulfillment_header_guard','fulfillment_line_guard',
   'fulfillment_create_shipment','returns_create','returns_receive','returns_refund_snapshot','returns_prepare_refund','returns_complete_refund','returns_lock_refund','returns_fail_refund',
   'orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','orders_refund_budget','inventory_restore_return',
   'auth_record_failure','guard_known_device','auth_fail_known_device','bootstrap_admin','auth_prune_attempts','auth_lock_login_user','auth_revoke_user_sessions',
   'auth_role_scopes','auth_create_api_key','auth_revoke_api_key','auth_touch_api_key','auth_revalidate_tenant',
   'orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations','orders_check_creation','outbox_active_tenants','outbox_metrics','notification_fanout','notification_quote_conversion_fanout','claim_notification_outbox','notification_begin','notification_finish','notification_redrive',
   'quote_actor_access','guard_quote','guard_quote_child','quote_outbox','create_quote','transition_quote','quote_conversion_guard','quote_conversion_lock','quote_conversion_bind','quote_conversion_get','checkout_guard',
   'customer_auth_context','customer_claim_guest_orders','customer_prepare_registration',
   'customer_consume_verification','customer_prepare_reset','customer_consume_reset','customer_save_session',
   'customer_session','customer_revoke_session','customer_admit','customer_failed',
   'resolve_fake_payment_account','resolve_stripe_payment_account','lock_payment_stream','admit_payment_inbox','finish_payment_inbox',
   'payments_guard_refund_attempt','payments_guard_refund_reversal','guard_payment_fact','guard_checkout_refund','payments_allocate_refund','payments_return_context','payments_lock_return_checkout','payments_bind_return_refund','payments_match_return_refund') LOOP
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
  AND NOT starts_with(p.proname,'data_rights_')
  AND NOT starts_with(p.proname,'tenant_lifecycle_')
  AND p.proname NOT LIKE 'cms_%' AND p.proname NOT IN ('catalog_listing_version','pricing_currency','pricing_write_list','pricing_write_entry','cart_tenant_active','cart_guard_version','inventory_context','inventory_immutable_movement','inventory_adjust',
   'inventory_reserve','inventory_reserve_owned','inventory_reserve_non_order','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
   'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order',
   'buyer_roles_seed_for_tenant','customer_credentials_touch_updated_at',
   'guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger','ledger_post_core',
   'payments_finalize_refund_core','payments_guard_refund_case','payments_retain_refund_case','payments_refund_case_evidence','payments_replay_refund_case','payments_resolve_refund_case','payments_open_refund_cases',
   'payments_post_refund_compensation','payments_guard_compensation_source','payments_assert_compensation_journal',
   'payments_finalize_refund','payments_refund_terminal','payments_post_return_refund','payments_guard_refund_terminal','orders_compensate_refund','returns_compensate_refund',
   'fulfillment_allocate','fulfillment_return_json','fulfillment_shipment_json','fulfillment_header_guard','fulfillment_line_guard',
   'fulfillment_create_shipment','returns_create','returns_receive','returns_refund_snapshot','returns_prepare_refund','returns_complete_refund','returns_lock_refund','returns_fail_refund',
   'orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','orders_refund_budget','inventory_restore_return',
   'auth_record_failure','guard_known_device','auth_fail_known_device','bootstrap_admin','auth_prune_attempts','auth_lock_login_user','auth_revoke_user_sessions',
   'auth_role_scopes','auth_create_api_key','auth_revoke_api_key','auth_touch_api_key','auth_revalidate_tenant',
   'orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations','orders_check_creation','outbox_active_tenants','outbox_metrics','notification_fanout','notification_quote_conversion_fanout','claim_notification_outbox','notification_begin','notification_finish','notification_redrive',
   'quote_actor_access','guard_quote','guard_quote_child','quote_outbox','create_quote','transition_quote','quote_conversion_guard','quote_conversion_lock','quote_conversion_bind','quote_conversion_get','checkout_guard',
   'customer_auth_context','customer_claim_guest_orders','customer_prepare_registration',
   'customer_consume_verification','customer_prepare_reset','customer_consume_reset','customer_save_session',
   'customer_session','customer_revoke_session','customer_admit','customer_failed',
   'resolve_fake_payment_account','resolve_stripe_payment_account','lock_payment_stream','admit_payment_inbox','finish_payment_inbox',
   'payments_guard_refund_attempt','payments_guard_refund_reversal','guard_payment_fact','guard_checkout_refund','payments_allocate_refund','payments_return_context','payments_lock_return_checkout','payments_bind_return_refund','payments_match_return_refund'))<>10 THEN
  RAISE EXCEPTION 'publisher/reliability function contract incomplete';
 END IF;
END $services$;
SELECT 'PASS: isolated service matrix and SECURITY DEFINER owner/search_path/EXECUTE' AS result;

-- Inventory owns quantity mutation; runtime is read+commands, manager alone adjusts.
DO $inventory$
DECLARE role_name text; r record; t record; a record; privilege text; f record;
 expected boolean; allowed_tables text[]:=ARRAY['warehouses','stock_levels','inventory_reservations','inventory_movements'];
 public_commands text[]:=ARRAY['inventory_reserve','inventory_reserve_non_order','inventory_release_non_order','inventory_commit_non_order'];
 names text[]:=ARRAY['inventory_context','inventory_immutable_movement','inventory_adjust',
  'inventory_reserve','inventory_reserve_owned','inventory_reserve_non_order','inventory_finish','inventory_release','inventory_commit','inventory_expire_due',
  'inventory_finish_non_order','inventory_release_non_order','inventory_commit_non_order','inventory_restore_return'];
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
    expected := privilege='SELECT' AND (t.relname=ANY(allowed_tables) OR (role_name='app_inventory_executor' AND t.relname IN('tenants','returns','return_lines','order_lines')))
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
   'app_media_service','app_publisher','app_publish_executor','app_inventory_manager','app_orders_executor','app_customer_auth','app_ledger_executor','app_queue_worker','app_outbox_tenant_broker','app_fulfillment_executor','app_returns_service'] LOOP
   expected := role_name='app_runtime' AND f.proname=ANY(public_commands)
    OR role_name='app_inventory_manager' AND f.proname='inventory_adjust'
    OR role_name='app_orders_executor' AND f.proname IN('inventory_commit','inventory_release')
    OR role_name='app_queue_worker' AND f.proname='inventory_expire_due'
    OR role_name='app_fulfillment_executor' AND f.proname='inventory_restore_return';
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'inventory routine authority mismatch: % %',role_name,f.proname;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND NOT p.proname=ANY(names) AND p.proname<>'cart_tenant_active' LOOP
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
 names text[]:=ARRAY['guard_ledger_account','guard_ledger_transaction','guard_ledger_entry','assert_ledger_balance','post_ledger','ledger_post_core'];
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
    'app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_customer_auth','app_orders_executor','app_queue_worker','app_outbox_tenant_broker','app_fulfillment_executor','app_returns_service'] LOOP
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM (role_name='app_runtime' AND f.proname='post_ledger')
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'ledger routine authority: % %',role_name,f.proname;
    END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'ledger has foreign routine capability: %',f.proname;
  END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names)
 OR NOT EXISTS(SELECT FROM pg_proc WHERE oid='app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[])'::regprocedure AND prorettype='uuid'::regtype) THEN
  RAISE EXCEPTION 'ledger routine contract incomplete';
 END IF;
END $ledger$;
SELECT 'PASS: ledger exact executor/runtime table-column-function capabilities and FORCE RLS' AS result;

-- Cart borrows only the existing unreachable executor's tenant-read privilege.
-- Runtime sees a boolean for its current context, never the tenant roster.
DO $cart_functions$
DECLARE f record; role_name text;
BEGIN
 SELECT p.* INTO STRICT f FROM pg_proc p WHERE p.oid='app.cart_tenant_active()'::regprocedure;
 IF NOT f.prosecdef OR f.proowner<>'app_inventory_executor'::regrole
  OR f.pronargs<>0 OR f.prorettype<>'boolean'::regtype OR f.provolatile<>'s'
  OR f.prolang<>(SELECT oid FROM pg_language WHERE lanname='sql')
  OR cardinality(f.proconfig) IS DISTINCT FROM 1
  OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
  OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
   WHERE grantee NOT IN ('app_runtime'::regrole,'app_inventory_executor'::regrole)
    OR privilege_type<>'EXECUTE' OR is_grantable) THEN
  RAISE EXCEPTION 'CART_UNSAFE_TENANT_HELPER';
 END IF;
 FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_domain_verifier',
  'app_media_service','app_publisher','app_publish_executor','app_outbox_worker',
  'app_reliability_executor','app_customer_auth','app_inventory_manager'] LOOP
  IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM (role_name='app_runtime')
   OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'CART_HELPER_EXECUTE_MISMATCH: %',role_name;
  END IF;
 END LOOP;
 IF has_table_privilege('app_runtime','app.tenants','SELECT')
  OR has_any_column_privilege('app_runtime','app.tenants','SELECT')
  OR has_schema_privilege('app_inventory_executor','app','CREATE')
  OR pg_has_role('app_runtime','app_inventory_executor','SET') THEN
  RAISE EXCEPTION 'CART_TENANT_CAPABILITY_BROADENED';
 END IF;
 SELECT p.* INTO STRICT f FROM pg_proc p WHERE p.oid='app.cart_guard_version()'::regprocedure;
 IF f.prokind<>'f' OR f.pronargs<>0 OR f.prorettype<>'trigger'::regtype OR f.prosecdef
  OR f.proowner<>(SELECT relowner FROM pg_class WHERE oid='app.carts'::regclass)
  OR cardinality(f.proconfig) IS DISTINCT FROM 1
  OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
  OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
   WHERE grantee<>f.proowner OR privilege_type<>'EXECUTE' OR is_grantable) THEN
  RAISE EXCEPTION 'CART_UNSAFE_VERSION_GUARD';
 END IF;
 FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  IF has_function_privilege(role_name,f.oid,'EXECUTE')
   OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'CART_GUARD_EXECUTE_MISMATCH: %',role_name;
  END IF;
 END LOOP;
END $cart_functions$;
SELECT 'PASS: cart tenant helper exact owner/search_path/boolean/STABLE/EXECUTE; no tenant-read grant' AS result;

-- Orders owns sealed snapshots. Its capability only locks reservations, terminates
-- them through inventory commands and appends reserved-namespace outbox events.
DO $orders$
DECLARE r record; t record; a record; f record; role_name text; privilege text;
 expected boolean; allowed text[];
 names text[]:=ARRAY['orders_context','orders_guard','orders_child_guard','orders_create','orders_transition','orders_lock_reservations','orders_check_creation','orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','orders_refund_budget','orders_compensate_refund'];
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
   expected:=privilege='SELECT' AND t.relname IN('orders','order_lines','order_adjustments','tenants','inventory_reservations','outbox_events','shipments','shipment_lines','returns','return_lines','payment_refund_terminals','payment_refund_reversals','ledger_transactions')
    OR privilege='INSERT' AND t.relname IN('orders','order_lines','order_adjustments');
   allowed:=ARRAY[]::text[];
   IF expected THEN SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='UPDATE' AND t.relname='orders' THEN allowed:=ARRAY['state','version','sealed','refunded_minor','updated_at'];
   -- UPDATE(id) permits row locking only; the return header trigger rejects actual Orders writes.
   ELSIF privilege='UPDATE' AND t.relname IN('inventory_reservations','returns') THEN allowed:=ARRAY['id'];
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
    'app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_customer_auth','app_ledger_executor','app_queue_worker','app_outbox_tenant_broker','app_fulfillment_executor','app_returns_service'] LOOP
    expected:=role_name='app_runtime' AND f.proname IN('orders_create','orders_transition','orders_lock_reservations','orders_check_creation')
     OR role_name='app_fulfillment_executor' AND f.proname IN('orders_context','orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','orders_refund_budget');
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

-- Queue ACL is a separate schema contract; upstream tables have no app tenant column.
DO $queue_matrix$
DECLARE role_name text; r record; t record; a record; f record; s record; privilege text;
 expected_table boolean; expected_column boolean; allowed text[];
 roles text[]:=ARRAY['app_runtime','app_queue_worker','app_customer_auth','app_ledger_executor','app_outbox_tenant_broker','app_auth','app_domain_resolver',
 'app_domain_verifier','app_media_service','app_publisher','app_publish_executor',
 'app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_orders_executor','app_credential_writer','app_admin_bootstrap','app_fulfillment_executor','app_returns_service'];
 tables text[]:=ARRAY['river_job','river_queue','river_leader','river_migration','river_notification'];
 insert_columns text[]:=ARRAY['id','args','created_at','kind','max_attempts','metadata','priority','queue','scheduled_at','state','tags','unique_key','unique_states'];
BEGIN
 IF (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='queue' AND c.relkind IN('r','p'))<>cardinality(tables)
 OR EXISTS(SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='queue' AND c.relkind IN('r','p') AND NOT c.relname=ANY(tables)) THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_TABLE_SET_INVALID'; END IF;
 IF (SELECT nspowner FROM pg_namespace WHERE nspname='queue') IS DISTINCT FROM 'app_river_migrator'::regrole THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_OWNER_INVALID'; END IF;
 FOREACH role_name IN ARRAY ARRAY['app_queue_worker','app_river_migrator'] LOOP
  SELECT * INTO STRICT r FROM pg_roles WHERE rolname=role_name;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
   OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid) THEN
   RAISE EXCEPTION 'QUEUE_MATRIX_UNSAFE_ROLE %',role_name; END IF;
 END LOOP;
 -- Effective transitive memberships, including an indirect NOLOGIN bridge.
 IF EXISTS(SELECT FROM pg_roles x WHERE NOT x.rolcanlogin AND NOT x.rolsuper
  AND (x.rolname<>'app_river_migrator' AND pg_has_role(x.oid,'app_river_migrator','MEMBER')
    OR x.rolname<>'app_queue_worker' AND pg_has_role(x.oid,'app_queue_worker','MEMBER'))) THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_TRANSITIVE_ROLE_ESCALATION'; END IF;
 IF pg_has_role('app_runtime','app_queue_worker','SET') OR pg_has_role('app_queue_worker','app_runtime','SET')
 OR pg_has_role('app_runtime','app_river_migrator','SET') OR pg_has_role('app_queue_worker','app_river_migrator','SET') THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_ROLE_ESCALATION'; END IF;
 IF NOT has_schema_privilege('app_queue_worker','app','USAGE') OR has_schema_privilege('app_queue_worker','app','CREATE') THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_APP_SCHEMA_INVALID'; END IF;
 IF NOT EXISTS(SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='queue' AND c.relname='river_job' AND c.relrowsecurity AND c.relforcerowsecurity) THEN
  RAISE EXCEPTION 'QUEUE_MATRIX_RLS_REQUIRED'; END IF;
 FOREACH role_name IN ARRAY roles LOOP
  IF has_schema_privilege(role_name,'queue','USAGE') IS DISTINCT FROM (role_name IN('app_runtime','app_queue_worker'))
  OR has_schema_privilege(role_name,'queue','CREATE') OR has_schema_privilege(role_name,'queue','USAGE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'QUEUE_MATRIX_SCHEMA_PRIVILEGE %',role_name; END IF;
  FOR t IN SELECT c.oid,c.relname,c.relowner,c.relacl FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='queue' AND c.relkind IN('r','p') LOOP
   IF t.relowner<>'app_river_migrator'::regrole
   OR EXISTS(SELECT FROM aclexplode(coalesce(t.relacl,acldefault('r',t.relowner))) WHERE grantee=0) THEN
    RAISE EXCEPTION 'QUEUE_MATRIX_TABLE_OWNER_PUBLIC %',t.relname; END IF;
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    expected_table:=role_name='app_runtime' AND t.relname='river_job' AND privilege='SELECT'
     OR role_name='app_queue_worker' AND (t.relname=ANY(ARRAY['river_job','river_queue','river_leader']) AND privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','DELETE']) OR t.relname='river_migration' AND privilege='SELECT');
    IF has_table_privilege(role_name,t.oid,privilege) IS DISTINCT FROM expected_table OR has_table_privilege(role_name,t.oid,privilege||' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'QUEUE_MATRIX_TABLE_PRIVILEGE % %.%',role_name,t.relname,privilege; END IF;
    IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
     allowed:=ARRAY[]::text[];
     IF role_name='app_runtime' AND t.relname='river_job' AND privilege='INSERT' THEN allowed:=insert_columns;
     ELSIF role_name='app_runtime' AND t.relname='river_job' AND privilege='UPDATE' THEN allowed:=ARRAY['kind']; END IF;
     FOR a IN SELECT attnum,attname,attacl FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
      expected_column:=expected_table OR a.attname::text=ANY(allowed);
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column
      OR has_column_privilege(role_name,t.oid,a.attnum,privilege||' WITH GRANT OPTION')
      OR EXISTS(SELECT FROM aclexplode(a.attacl) WHERE grantee=0) THEN
       RAISE EXCEPTION 'QUEUE_MATRIX_COLUMN_PRIVILEGE % %.% %',role_name,t.relname,a.attname,privilege; END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
  FOR s IN SELECT c.oid,c.relname,c.relowner,c.relacl FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='queue' AND c.relkind='S' LOOP
   IF s.relowner<>'app_river_migrator'::regrole OR EXISTS(SELECT FROM aclexplode(coalesce(s.relacl,acldefault('s',s.relowner))) WHERE grantee=0) THEN
    RAISE EXCEPTION 'QUEUE_MATRIX_SEQUENCE_OWNER_PUBLIC'; END IF;
   FOREACH privilege IN ARRAY ARRAY['USAGE','SELECT','UPDATE'] LOOP
    expected_table:=role_name IN('app_runtime','app_queue_worker') AND s.relname='river_job_id_seq' AND privilege='USAGE';
    IF has_sequence_privilege(role_name,s.oid,privilege) IS DISTINCT FROM expected_table OR has_sequence_privilege(role_name,s.oid,privilege||' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'QUEUE_MATRIX_SEQUENCE_PRIVILEGE % % %',role_name,s.relname,privilege; END IF;
   END LOOP;
  END LOOP;
  FOR f IN SELECT p.oid,p.proname,p.proowner,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='queue' LOOP
   expected_table:=role_name IN('app_runtime','app_queue_worker') AND f.oid='queue.river_job_state_in_bitmask(bit,queue.river_job_state)'::regprocedure;
   IF f.proowner<>'app_river_migrator'::regrole OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')
   OR has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected_table OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'QUEUE_MATRIX_FUNCTION_PRIVILEGE % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 -- Worker has only the controlled inventory expiry business command, no table ACL.
 FOR t IN SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   IF has_table_privilege('app_queue_worker',t.oid,privilege) OR has_table_privilege('app_queue_worker',t.oid,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'QUEUE_MATRIX_BUSINESS_TABLE_PRIVILEGE'; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege('app_queue_worker',t.oid,a.attnum,privilege) THEN RAISE EXCEPTION 'QUEUE_MATRIX_BUSINESS_COLUMN_PRIVILEGE'; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF has_function_privilege('app_queue_worker',f.oid,'EXECUTE') IS DISTINCT FROM (f.oid='app.inventory_expire_due(integer)'::regprocedure)
  OR has_function_privilege('app_queue_worker',f.oid,'EXECUTE WITH GRANT OPTION') THEN RAISE EXCEPTION 'QUEUE_MATRIX_BUSINESS_FUNCTION_PRIVILEGE'; END IF;
 END LOOP;
END $queue_matrix$;
SELECT 'PASS: queue exact table-column-sequence-function matrix and isolated worker capability' AS result;

-- Outbox broker grants work tenant IDs and aggregate gauges, never event content.
DO $discovery_matrix$
DECLARE r record; t record; a record; f record; privilege text; expected boolean; role_name text;
 roles text[]:=ARRAY['app_runtime','app_queue_worker','app_customer_auth','app_ledger_executor','app_auth','app_domain_resolver','app_domain_verifier','app_media_service','app_publisher','app_publish_executor','app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor','app_outbox_tenant_broker','app_orders_executor','app_fulfillment_executor','app_returns_service'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_outbox_tenant_broker';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 OR NOT has_schema_privilege(r.oid,'app','USAGE') OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_UNSAFE_ROLE'; END IF;
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   IF has_table_privilege(r.oid,t.oid,privilege) OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_TABLE_CAPABILITY'; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     expected:=t.relname='outbox_events' AND privilege='SELECT' AND a.attname=ANY(ARRAY['tenant_id','status','created_at']);
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM expected OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_COLUMN_CAPABILITY'; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.* FROM pg_proc p WHERE p.oid=ANY(ARRAY[
  'app.outbox_active_tenants(uuid,integer)'::regprocedure,'app.outbox_metrics()'::regprocedure]) LOOP
  IF f.proowner<>r.oid OR NOT f.prosecdef OR f.proretset IS DISTINCT FROM true OR cardinality(f.proconfig) IS DISTINCT FROM 1
  OR replace(f.proconfig[1],' ','') IS DISTINCT FROM (CASE WHEN f.proname='outbox_metrics' THEN 'search_path=pg_catalog,app' ELSE 'search_path=pg_catalog,app,pg_temp' END)
  OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0) THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_ROUTINE_OWNER'; END IF;
  FOREACH role_name IN ARRAY roles LOOP
   expected:=role_name=ANY(ARRAY['app_outbox_worker','app_outbox_tenant_broker']);
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') IS DISTINCT FROM (role_name='app_outbox_tenant_broker') THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_EXECUTE_CAPABILITY: % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF has_function_privilege(r.oid,f.oid,'EXECUTE') IS DISTINCT FROM (f.oid=ANY(ARRAY['app.outbox_active_tenants(uuid,integer)'::regprocedure,'app.outbox_metrics()'::regprocedure])) THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_FOREIGN_ROUTINE'; END IF;
 END LOOP;
 IF NOT EXISTS(SELECT FROM pg_policy p WHERE p.polrelid='app.outbox_events'::regclass AND p.polname='outbox_tenant_discovery' AND p.polcmd='r' AND p.polroles=ARRAY[r.oid] AND pg_get_expr(p.polqual,p.polrelid)='true' AND p.polwithcheck IS NULL) THEN RAISE EXCEPTION 'OUTBOX_DISCOVERY_POLICY'; END IF;
END $discovery_matrix$;
SELECT 'PASS: outbox broker exact discovery/aggregate table-column-routine/RLS matrix' AS result;

-- Notifications executor has only exact frozen-envelope and fenced-state columns.
DO $notifications$
DECLARE r record;t record;a record;f record; privilege text;cols text[]; expected boolean; role_name text;
 names text[]:=ARRAY['notification_fanout','notification_quote_conversion_fanout','claim_notification_outbox','notification_begin','notification_finish','notification_redrive']; BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_notifications_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid) OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_UNSAFE_ROLE'; END IF;
 FOR t IN SELECT c.* FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  IF t.relname IN('notification_deliveries','notification_redrive_requests') AND (NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_RLS'; END IF;
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=privilege='SELECT' AND t.relname IN('notification_deliveries','notification_redrive_requests','outbox_events');
   cols:=ARRAY[]::text[];
   IF expected THEN SELECT array_agg(attname::text) INTO cols FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='SELECT' AND t.relname='orders' THEN cols:=ARRAY['tenant_id','id','customer_id','guest_contact'];
   ELSIF privilege='SELECT' AND t.relname='customers' THEN cols:=ARRAY['tenant_id','id','email_normalized'];
   ELSIF privilege='SELECT' AND t.relname='quotes' THEN cols:=ARRAY['tenant_id','id','site_id','inquiry_id','buyer_company_id','accepted_by','status','version'];
   ELSIF privilege='SELECT' AND t.relname='inquiries' THEN cols:=ARRAY['tenant_id','id','site_id','buyer_company_id','buyer_email'];
   ELSIF privilege='SELECT' AND t.relname='quote_conversions' THEN cols:=ARRAY['tenant_id','quote_id','order_id','quote_version','customer_id'];
   ELSIF privilege='INSERT' AND t.relname='notification_deliveries' THEN cols:=ARRAY['tenant_id','id','source_event_id','dispatch_event_id','recipient','locale','template_key','reference_id','status','last_error'];
   ELSIF privilege='INSERT' AND t.relname='notification_redrive_requests' THEN cols:=ARRAY['tenant_id','notification_id','idempotency_key','generation','dispatch_event_id'];
   ELSIF privilege='INSERT' AND t.relname='outbox_events' THEN cols:=ARRAY['tenant_id','id','event_type','payload','delivery_target','dedup_key'];
   ELSIF privilege='UPDATE' AND t.relname='notification_deliveries' THEN cols:=ARRAY['dispatch_event_id','status','attempt_count','generation','send_token','lease_attempt','lease_worker','last_error','updated_at','sent_at'];
   ELSIF privilege='UPDATE' AND t.relname='outbox_events' THEN cols:=ARRAY['status','lease_owner','lease_until','attempts','last_error']; END IF;
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_TABLE % %',t.relname,privilege; END IF;
   IF privilege IN('SELECT','INSERT','UPDATE','REFERENCES') THEN
    FOR a IN SELECT attname,attnum FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM (a.attname=ANY(cols)) OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_COLUMN %.% %',t.relname,a.attname,privilege; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.* FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF f.proname=ANY(names) THEN
   IF NOT f.prosecdef OR f.proowner<>r.oid OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','')<>'search_path=pg_catalog,app,pg_temp' OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_FUNCTION %',f.proname; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
    expected:=role_name='app_notifications_executor' OR role_name='app_runtime' AND f.proname='notification_redrive' OR role_name='app_outbox_worker' AND f.proname IN('claim_notification_outbox','notification_begin','notification_finish');
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') IS DISTINCT FROM (role_name='app_notifications_executor') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_EXECUTE % %',role_name,f.proname; END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_FOREIGN_FUNCTION %',f.proname; END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p WHERE p.pronamespace='app'::regnamespace AND p.proname=ANY(names))<>cardinality(names) THEN RAISE EXCEPTION 'NOTIFICATIONS_MATRIX_INCOMPLETE'; END IF;
END $notifications$;
SELECT 'PASS: notifications exact column/function privileges and FORCE RLS' AS result;
-- C-22a: exact controlled executor matrix including every column, independent
-- of the migration grant statements. No login can assume the privacy role.
DO $privacy_matrix$
DECLARE
 r record; t record; a record; privilege text; expected boolean; cols text[];
 selected text[]:=ARRAY['tenants','customers','customer_addresses','buyer_company_members','orders','order_lines','carts','cart_lines','data_rights_exports','data_rights_erasures','data_rights_erased_orders','notification_deliveries','shipments','returns','inventory_movements'];
 commands text[]:=ARRAY['data_rights_artifact_live','data_rights_request_export','data_rights_status','data_rights_build_export','data_rights_complete_export','data_rights_download_info','data_rights_consume_export','data_rights_erase_customer','data_rights_download_deadline','data_rights_worker_info'];
 f record; role_name text;
 private_functions text[]:=ARRAY['data_rights_receipt_owned','data_rights_context','data_rights_tax_scrub','data_rights_lock_export','data_rights_line_guard','data_rights_erasure_guard','data_rights_credential_guard','data_rights_order_customer_guard','data_rights_customer_guard','data_rights_notification_insert_guard','data_rights_notification_guard','data_rights_notification_dispatch_insert_guard','data_rights_notification_dispatch_guard','data_rights_notification_owner','data_rights_quote_scrub','data_rights_scrub_values','data_rights_email_namespace_guard','data_rights_scrub_guard','data_rights_checkout_erased_guard','data_rights_customer_orders','data_rights_fulfillment_erased_guard'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_data_rights_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
  OR EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid OR member=r.oid)
  OR has_schema_privilege(r.oid,'app','CREATE') OR has_schema_privilege(r.oid,'queue','CREATE') THEN
  RAISE EXCEPTION 'privacy executor is reachable or can perform DDL'; END IF;
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=privilege='SELECT' AND t.relname=ANY(selected)
    OR privilege='INSERT' AND t.relname=ANY(ARRAY['data_rights_exports','data_rights_erasures','data_rights_erased_orders','audit_events'])
    OR privilege='UPDATE' AND t.relname='data_rights_exports'
    OR privilege='DELETE' AND t.relname=ANY(ARRAY['customer_addresses','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links']);
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'privacy table privilege mismatch app.% %',t.relname,privilege; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    cols:=ARRAY[]::text[];
    IF NOT expected THEN
     IF privilege='SELECT' AND t.relname=ANY(ARRAY['customer_credentials','customer_auth_tokens','customer_sessions']) THEN cols:=ARRAY['tenant_id','customer_id'];
     ELSIF privilege='SELECT' AND t.relname='customer_guest_order_links' THEN cols:=ARRAY['tenant_id','customer_id','order_id'];
     ELSIF privilege='SELECT' AND t.relname='quotes' THEN cols:=ARRAY['tenant_id','id','accepted_by','snapshot'];
     ELSIF privilege='SELECT' AND t.relname='quote_conversions' THEN cols:=ARRAY['tenant_id','quote_id','customer_id'];
     ELSIF privilege='SELECT' AND t.relname='checkout_sessions' THEN cols:=ARRAY['tenant_id','id','order_id','start_result','final_result'];
     ELSIF privilege='SELECT' AND t.relname='checkout_attempts' THEN cols:=ARRAY['tenant_id','event_kind','event_key','checkout_id','result'];
     ELSIF privilege='SELECT' AND t.relname='quote_lines' THEN cols:=ARRAY['tenant_id','quote_id','line_no'];
     ELSIF privilege='SELECT' AND t.relname='order_adjustments' THEN cols:=ARRAY['tenant_id','id','order_id'];
     ELSIF privilege='SELECT' AND t.relname='idempotency_records' THEN cols:=ARRAY['tenant_id','actor_kind','actor_id','operation','idempotency_key','response_body','response_body_reference'];
     ELSIF privilege='SELECT' AND t.relname='outbox_events' THEN cols:=ARRAY['tenant_id','id','event_type','delivery_target','payload','status','lease_owner','lease_until','last_error'];
     ELSIF privilege='UPDATE' THEN cols:=CASE t.relname WHEN 'customers' THEN ARRAY['email','display_name','status'] WHEN 'buyer_company_members' THEN ARRAY['status'] WHEN 'orders' THEN ARRAY['shipping_snapshot','guest_contact'] WHEN 'order_lines' THEN ARRAY['tax_snapshot'] WHEN 'notification_deliveries' THEN ARRAY['recipient','status','last_error','send_token','lease_attempt','lease_worker','sent_at','updated_at'] WHEN 'outbox_events' THEN ARRAY['status','lease_owner','lease_until','last_error'] WHEN 'checkout_sessions' THEN ARRAY['address_snapshot','start_result','final_result'] WHEN 'checkout_attempts' THEN ARRAY['result'] WHEN 'quotes' THEN ARRAY['snapshot'] WHEN 'quote_lines' THEN ARRAY['description','snapshot'] WHEN 'order_adjustments' THEN ARRAY['reason'] WHEN 'idempotency_records' THEN ARRAY['response_body','response_body_reference'] WHEN 'shipments' THEN ARRAY['carrier','tracking_number'] WHEN 'returns' THEN ARRAY['reason','creation_response','receive_response'] WHEN 'inventory_movements' THEN ARRAY['reason'] ELSE ARRAY[]::text[] END; END IF;
    END IF;
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM (expected OR a.attname::text=ANY(cols))
      OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
      RAISE EXCEPTION 'privacy column privilege mismatch app.%.% %',t.relname,a.attname,privilege; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND starts_with(p.proname,'data_rights_'))<>cardinality(commands||private_functions) THEN RAISE EXCEPTION 'privacy function inventory incomplete'; END IF;
 FOR f IN SELECT p.oid,p.proname,p.proowner,p.prosecdef,p.proconfig FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND starts_with(p.proname,'data_rights_') LOOP
  IF NOT f.proname=ANY(commands||private_functions) OR f.proowner<>r.oid
   OR f.prosecdef IS DISTINCT FROM (f.proname=ANY(commands||ARRAY['data_rights_context','data_rights_lock_export','data_rights_credential_guard','data_rights_order_customer_guard','data_rights_notification_insert_guard','data_rights_notification_dispatch_insert_guard','data_rights_fulfillment_erased_guard']))
   OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR cardinality(f.proconfig) IS DISTINCT FROM (CASE WHEN f.proname IN('data_rights_credential_guard','data_rights_order_customer_guard','data_rights_notification_insert_guard','data_rights_notification_dispatch_insert_guard') THEN 2 ELSE 1 END) THEN RAISE EXCEPTION 'unsafe privacy function %',f.proname; END IF;
  IF EXISTS(SELECT FROM aclexplode(coalesce((SELECT proacl FROM pg_proc WHERE oid=f.oid),acldefault('f',r.oid))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN RAISE EXCEPTION 'public privacy function capability'; END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_data_rights_executor' LOOP
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM (role_name='app_runtime' AND f.proname=ANY(commands))
    OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'privacy function privilege mismatch % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
END $privacy_matrix$;

-- Quotes uses a non-login, non-inheritable command capability with no identity secrets.
DO $quotes$
DECLARE r record; t record; a record; f record; privilege text; role_name text;
 expected_table boolean; expected_column boolean; allowed text[];
 names text[]:=ARRAY['quote_actor_access','guard_quote','guard_quote_child','quote_outbox','create_quote','transition_quote','quote_conversion_guard','quote_conversion_lock','quote_conversion_bind','quote_conversion_get'];
 public_names text[]:=ARRAY['quote_actor_access','create_quote','transition_quote','quote_conversion_lock','quote_conversion_bind','quote_conversion_get'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_quotes_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 OR pg_has_role('app_runtime',r.oid,'SET') OR has_schema_privilege(r.oid,'app','CREATE')
 OR NOT has_schema_privilege(r.oid,'app','USAGE') OR has_schema_privilege(r.oid,'app','USAGE WITH GRANT OPTION') THEN
  RAISE EXCEPTION 'QUOTES_UNSAFE_EXECUTOR'; END IF;
 FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  IF t.relname=ANY(ARRAY['inquiries','quotes','quote_lines','quote_approvals','quote_conversions']) AND (NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN RAISE EXCEPTION 'QUOTES_FORCE_RLS %',t.relname; END IF;
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected_table:=privilege='SELECT' AND t.relname=ANY(ARRAY['quotes','quote_lines','quote_approvals','quote_conversions']);
   allowed:=ARRAY[]::text[];
   IF expected_table THEN
    SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='SELECT' THEN
    allowed:=CASE t.relname WHEN 'users' THEN ARRAY['id','status'] WHEN 'tenants' THEN ARRAY['id','status']
     WHEN 'tenant_memberships' THEN ARRAY['tenant_id','user_id','role','status']
     WHEN 'inquiries' THEN ARRAY['tenant_id','site_id','id','buyer_company_id']
     WHEN 'buyer_companies' THEN ARRAY['tenant_id','id','status'] WHEN 'customers' THEN ARRAY['tenant_id','id','status']
     WHEN 'buyer_company_members' THEN ARRAY['tenant_id','buyer_company_id','customer_id','role_code','status']
     WHEN 'orders' THEN ARRAY['tenant_id','id','customer_id','order_number','currency','items_net_minor','tax_minor','shipping_minor','shipping_snapshot','total_minor','sealed','state']
     WHEN 'order_lines' THEN ARRAY['tenant_id','order_id','sku_id','quantity','unit_price_minor','line_net_minor','line_tax_minor','line_total_minor','tax_snapshot']
     WHEN 'order_adjustments' THEN ARRAY['tenant_id','order_id'] ELSE allowed END;
   ELSIF privilege='INSERT' THEN
    allowed:=CASE t.relname WHEN 'quote_conversions' THEN ARRAY['tenant_id','quote_id','buyer_company_id','customer_id','order_id','quote_version','idempotency_key','request_hash','order_number','currency','total_minor'] WHEN 'quotes' THEN ARRAY['tenant_id','id','site_id','inquiry_id','buyer_company_id','created_by','currency','subtotal','tax','shipping','total','snapshot','expires_at']
     WHEN 'quote_lines' THEN ARRAY['tenant_id','quote_id','line_no','product_id','variant_id','description','quantity','unit_price','line_total','snapshot']
     WHEN 'quote_approvals' THEN ARRAY['tenant_id','quote_id','buyer_company_id','requested_by','decided_by','decision']
     WHEN 'outbox_events' THEN ARRAY['tenant_id','event_type','payload','delivery_target','dedup_key'] ELSE allowed END;
   ELSIF privilege='UPDATE' AND t.relname='quotes' THEN allowed:=ARRAY['status','version','accepted_by','updated_at']; END IF;
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected_table OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'QUOTES_TABLE_CAPABILITY % %',t.relname,privilege; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     expected_column:=a.attname::text=ANY(allowed);
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'QUOTES_COLUMN_CAPABILITY %.% %',t.relname,a.attname,privilege; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.proowner,p.prosecdef,p.proconfig,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' LOOP
  IF f.proname=ANY(names) THEN
   IF f.proowner<>r.oid OR f.prosecdef IS DISTINCT FROM (f.proname=ANY(public_names))
   OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN RAISE EXCEPTION 'QUOTES_UNSAFE_ROUTINE %',f.proname; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_quotes_executor' LOOP
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM (role_name='app_runtime' AND f.proname=ANY(public_names)) OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN RAISE EXCEPTION 'QUOTES_ROUTINE_CAPABILITY % %',role_name,f.proname; END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'QUOTES_FOREIGN_ROUTINE %',f.proname; END IF;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN RAISE EXCEPTION 'QUOTES_ROUTINE_CONTRACT_INCOMPLETE'; END IF;
END $quotes$;
SELECT 'PASS: quotes exact table/column/function grants, bounded definer authority and FORCE RLS' AS result;
-- CMS controlled functions and capability flag must have exact effective ACLs.
DO $cms_matrix$
DECLARE f record; role_name text; expected boolean;
 names text[]:=ARRAY['cms_context','cms_revision_immutable','cms_draft_guard','cms_validate_document','cms_products','cms_read_draft','cms_create_draft','cms_apply','cms_publish','cms_restore','cms_release_history',
  'cms_release_skus_immutable','cms_seal_skus','cms_serving_put','cms_serving_invalidate'];
 public_names text[]:=ARRAY['cms_read_draft','cms_create_draft','cms_apply','cms_publish','cms_restore','cms_release_history','cms_seal_skus','cms_serving_put'];
BEGIN
 IF (SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname LIKE 'cms_%')<>cardinality(names) THEN RAISE EXCEPTION 'CMS_MATRIX_ROUTINE_SET'; END IF;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname LIKE 'cms_%' LOOP
  IF NOT f.proname=ANY(names) OR f.proowner<>'app_publish_executor'::regrole
   OR f.prosecdef IS DISTINCT FROM (f.proname=ANY(public_names) OR f.proname='cms_serving_invalidate') OR cardinality(f.proconfig)<>1
   OR replace(f.proconfig[1],' ','')<>'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN RAISE EXCEPTION 'CMS_MATRIX_ROUTINE_DEFINITION'; END IF;
  -- Every app_* role (including later capabilities) has an exact expectation.
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   expected:=role_name='app_publish_executor' OR role_name='app_runtime' AND f.proname=ANY(public_names);
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected OR (role_name<>'app_publish_executor' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION')) THEN RAISE EXCEPTION 'CMS_MATRIX_EXECUTE: % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 IF has_column_privilege('app_runtime','app.site_drafts','is_cms','INSERT') OR has_column_privilege('app_runtime','app.site_drafts','is_cms','UPDATE')
  OR has_column_privilege('app_runtime','app.sites','publication_revision','INSERT') OR has_column_privilege('app_runtime','app.sites','publication_revision','UPDATE') THEN RAISE EXCEPTION 'CMS_MATRIX_FLAG_AUTHORITY'; END IF;
 IF EXISTS(SELECT FROM pg_class WHERE oid IN ('app.draft_page_media'::regclass,'app.site_release_skus'::regclass,'app.storefront_serving'::regclass,'app.serving_invalidations'::regclass) AND NOT (relrowsecurity AND relforcerowsecurity)) THEN RAISE EXCEPTION 'CMS_MATRIX_FORCE_RLS'; END IF;
END $cms_matrix$;
SELECT 'PASS: CMS exact routine/capability flag/FORCE RLS matrix' AS result;
-- Checkout uses an invoker trigger, not a new capability role: nobody in the
-- application/service role graph may execute it directly or grant EXECUTE.
DO $checkout_capability$
DECLARE f record; role_name text;
BEGIN
 SELECT p.oid,p.prosecdef,p.proconfig,p.proacl,p.proowner INTO STRICT f
 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='app' AND p.proname='checkout_guard' AND p.pronargs=0;
 IF f.prosecdef OR cardinality(f.proconfig) IS DISTINCT FROM 1
 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
 OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
  RAISE EXCEPTION 'CHECKOUT_GATE unsafe trigger capability';
 END IF;
 FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_customer_auth',
 'app_domain_verifier','app_media_service','app_publisher','app_publish_executor','app_outbox_worker',
 'app_reliability_executor','app_inventory_manager','app_inventory_executor','app_ledger_executor','app_orders_executor',
 'app_queue_worker','app_river_migrator','app_outbox_tenant_broker','app_notifications_executor','app_quotes_executor','app_credential_writer','app_admin_bootstrap','app_fulfillment_executor','app_returns_service'] LOOP
  IF has_function_privilege(role_name,f.oid,'EXECUTE') OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'CHECKOUT_GATE unexpected trigger executor %',role_name;
  END IF;
 END LOOP;
END $checkout_capability$;
SELECT 'PASS: checkout exact table/column grants and private invoker trigger' AS result;

-- Independent least-privilege expectations for the customer proof executor.
DO $customer_executor$
DECLARE r record; t record; a record; f record; role_name text; privilege text;
 allowed text[]; expected_table boolean; expected_column boolean;
 signatures text[]:=ARRAY['app.customer_auth_context()', 'app.customer_claim_guest_orders(uuid)',
 'app.customer_prepare_registration(text,text,bytea,timestamp with time zone)',
 'app.customer_consume_verification(bytea)','app.customer_prepare_reset(text,bytea,timestamp with time zone)',
 'app.customer_consume_reset(bytea,text)','app.customer_save_session(uuid,text,bytea,bytea,timestamp with time zone,timestamp with time zone)',
 'app.customer_session(bytea,bigint)','app.customer_revoke_session(bytea)',
 'app.customer_admit(bytea,bytea,timestamp with time zone)','app.customer_failed(bytea,bytea)'];
 signature text; expected_functions oid[]:=ARRAY[]::oid[];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_customer_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
  OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
  OR has_schema_privilege(r.oid,'app','CREATE') OR NOT has_schema_privilege(r.oid,'app','USAGE')
  OR has_schema_privilege(r.oid,'app','USAGE WITH GRANT OPTION') THEN RAISE EXCEPTION 'CUSTOMER_AUTH_EXECUTOR_UNSAFE'; END IF;
 FOREACH signature IN ARRAY signatures LOOP
  IF to_regprocedure(signature) IS NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_ROUTINE_MISSING %',signature; END IF;
  expected_functions:=array_append(expected_functions,to_regprocedure(signature)::oid);
 END LOOP;
 FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_class c
  WHERE c.relnamespace='app'::regnamespace AND c.relkind IN('r','p') LOOP
  IF t.relname=ANY(ARRAY['customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits'])
   AND(NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_FORCE_RLS %',t.relname; END IF;
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   allowed:=ARRAY[]::text[];
   expected_table:=privilege='SELECT' AND t.relname=ANY(ARRAY['customers','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits','customer_auth_global_limits']);
   IF expected_table THEN
    SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='SELECT' THEN
    allowed:=CASE t.relname WHEN 'tenants' THEN ARRAY['id','status'] WHEN 'orders' THEN ARRAY['tenant_id','id','customer_id','guest_contact','sealed'] ELSE allowed END;
   ELSIF privilege='INSERT' THEN
    allowed:=CASE t.relname WHEN 'customers' THEN ARRAY['tenant_id','id','email','display_name']
     WHEN 'customer_credentials' THEN ARRAY['tenant_id','customer_id','password_hash']
     WHEN 'customer_auth_tokens' THEN ARRAY['tenant_id','customer_id','token_hash','purpose','email_normalized','pending_password_hash','credential_version','expires_at']
     WHEN 'customer_sessions' THEN ARRAY['tenant_id','customer_id','token_hash','credential_version','expires_at','idle_expires_at']
     WHEN 'customer_guest_order_links' THEN ARRAY['tenant_id','customer_id','order_id']
     WHEN 'customer_auth_limits' THEN ARRAY['tenant_id','kind','key_hash']
     WHEN 'customer_auth_global_limits' THEN ARRAY['kind','key_hash'] ELSE allowed END;
   ELSIF privilege='UPDATE' THEN
    allowed:=CASE t.relname WHEN 'customers' THEN ARRAY['verified_at'] WHEN 'customer_credentials' THEN ARRAY['password_hash']
     WHEN 'customer_auth_tokens' THEN ARRAY['consumed_at'] WHEN 'customer_sessions' THEN ARRAY['revoked_at','idle_expires_at','last_seen_at']
     WHEN 'customer_auth_limits' THEN ARRAY['window_start','attempts','failures','cooldown_until']
     WHEN 'customer_auth_global_limits' THEN ARRAY['window_start','attempts','failures','cooldown_until'] ELSE allowed END;
   END IF;
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected_table OR has_table_privilege(r.oid,t.oid,privilege||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'CUSTOMER_AUTH_TABLE_CAPABILITY % %',t.relname,privilege; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     expected_column:=a.attname::text=ANY(allowed);
     IF has_column_privilege(r.oid,t.oid,a.attnum,privilege) IS DISTINCT FROM expected_column OR has_column_privilege(r.oid,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
      RAISE EXCEPTION 'CUSTOMER_AUTH_COLUMN_CAPABILITY %.% %',t.relname,a.attname,privilege; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  IF f.oid=ANY(expected_functions) THEN
   IF f.proowner<>r.oid OR f.prosecdef IS DISTINCT FROM(f.proname<>'customer_auth_context')
    OR f.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::text[]
    OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
    RAISE EXCEPTION 'CUSTOMER_AUTH_UNSAFE_ROUTINE %',f.proname; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_customer_executor' LOOP
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM(role_name='app_customer_auth' AND f.proname<>'customer_auth_context')
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'CUSTOMER_AUTH_ROUTINE_CAPABILITY % %',role_name,f.proname; END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'CUSTOMER_AUTH_FOREIGN_ROUTINE %',f.proname; END IF;
 END LOOP;
END $customer_executor$;
SELECT 'PASS: exact customer function signatures, executor isolation and column privileges';

-- Payment resolver is a NOLOGIN account-directory capability only. Check every
-- table/column and every function, including inherited/PUBLIC privileges.
DO $payments_matrix$
DECLARE r record; t record; c record; f record; privilege text; expected boolean; role_name text;
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_payment_resolver';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_UNSAFE_ROLE'; END IF;
 FOR t IN SELECT oid,relname FROM pg_class WHERE relnamespace='app'::regnamespace AND relkind IN('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=t.relname='payment_provider_accounts' AND privilege='SELECT';
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_TABLE % %',t.relname,privilege; END IF;
  END LOOP;
  FOR c IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','REFERENCES'] LOOP
    expected:=t.relname='payment_provider_accounts' AND (privilege='SELECT' OR privilege='UPDATE' AND c.attname='enabled');
    IF has_column_privilege(r.oid,t.oid,c.attnum,privilege) IS DISTINCT FROM expected THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_COLUMN % % %',t.relname,c.attname,privilege; END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  IF has_function_privilege(r.oid,f.oid,'EXECUTE') IS DISTINCT FROM (f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account')) THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_FUNCTION %',f.proname; END IF;
  IF f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account') THEN
   IF NOT f.prosecdef OR f.proowner<>r.oid OR NOT f.proconfig @> ARRAY['search_path=pg_catalog, app, pg_temp'] THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_RESOLVER'; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
    expected:=role_name IN('app_runtime','app_payment_resolver','app_payments_service','app_payments_ingress');
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_RESOLVE_GRANT %',role_name; END IF;
   END LOOP;
  END IF;
 END LOOP;
 -- The generic legacy capability inventory above excludes exactly this list;
 -- retain exact signatures, invoker/definer, owner, search_path and ACL assertions.
 FOR f IN SELECT p.*, n.expected_signature FROM (VALUES
  ('resolve_fake_payment_account(text)'),('resolve_stripe_payment_account(text)'),('lock_payment_stream(text,text)'),
  ('admit_payment_inbox(text,text,bytea)'),('finish_payment_inbox(uuid,boolean)'),
  ('guard_payment_fact()'),('guard_checkout_refund()')) AS n(expected_signature)
 LEFT JOIN pg_proc p ON p.oid=to_regprocedure('app.'||n.expected_signature) LOOP
  IF f.oid IS NULL OR f.prosecdef IS DISTINCT FROM (f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account'))
   OR f.proowner IS DISTINCT FROM (CASE WHEN f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account') THEN r.oid
      ELSE (SELECT relowner FROM pg_class WHERE oid='app.payment_events'::regclass) END)
   OR cardinality(f.proconfig) IS DISTINCT FROM 1
   OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE')
  THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_ROUTINE %',f.expected_signature; END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   expected:=(role_name='app_runtime' AND f.proname IN
    ('resolve_fake_payment_account','resolve_stripe_payment_account','lock_payment_stream','admit_payment_inbox','finish_payment_inbox'))
    OR (role_name IN('app_payment_resolver','app_payments_ingress') AND f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account'))
    OR (role_name='app_payments_service' AND f.proname IN('resolve_fake_payment_account','resolve_stripe_payment_account','lock_payment_stream','admit_payment_inbox','finish_payment_inbox'));
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR (role_name<>'app_payment_resolver' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION'))
   THEN RAISE EXCEPTION 'PAYMENTS_MATRIX_ROUTINE_ACL % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
END $payments_matrix$;
-- Fulfillment owns sealed quantity records; trusted Returns service coordinates
-- PSP+public Ledger only, with no direct writes or inheritance path to any owner.
DO $fulfillment_matrix$
DECLARE r record;t record;a record;f record;role_name text;privilege text;
 expected_table boolean;allowed text[];expected boolean;
 tables text[]:=ARRAY['shipments','shipment_lines','returns','return_lines'];
 names text[]:=ARRAY['fulfillment_allocate','fulfillment_return_json','fulfillment_shipment_json',
  'fulfillment_header_guard','fulfillment_line_guard','fulfillment_create_shipment','returns_create',
  'returns_receive','returns_refund_snapshot','returns_prepare_refund','returns_complete_refund','returns_lock_refund','returns_fail_refund','returns_compensate_refund'];
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_fulfillment_executor','app_returns_service'] LOOP
  SELECT * INTO STRICT r FROM pg_roles WHERE rolname=role_name;
  IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
   OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid)
   OR role_name='app_fulfillment_executor' AND EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid)
   OR pg_has_role('app_runtime',r.oid,'SET') OR pg_has_role('app_auth',r.oid,'SET')
   OR has_schema_privilege(role_name,'app','CREATE') OR NOT has_schema_privilege(role_name,'app','USAGE')
   OR has_schema_privilege(role_name,'app','USAGE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'FULFILLMENT_MATRIX_UNSAFE_ROLE %',role_name; END IF;
  FOR t IN SELECT c.oid,c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
   IF t.relname=ANY(tables) AND(NOT t.relrowsecurity OR NOT t.relforcerowsecurity) THEN
    RAISE EXCEPTION 'FULFILLMENT_MATRIX_FORCE_RLS %',t.relname; END IF;
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    expected_table:=false;allowed:=ARRAY[]::text[];
    IF privilege='SELECT' AND(role_name='app_fulfillment_executor' AND t.relname=ANY(tables||ARRAY['tenants','orders','order_lines',
      'inventory_reservations','ledger_accounts','ledger_transactions','ledger_entries','payment_refund_terminals','payment_refund_reversals'])
      OR role_name='app_returns_service' AND t.relname IN('returns','return_lines'))
     OR privilege='INSERT' AND role_name='app_fulfillment_executor' AND t.relname=ANY(tables) THEN
     expected_table:=true;
     SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
    ELSIF privilege='INSERT' AND role_name='app_fulfillment_executor' AND t.relname='outbox_events' THEN
     allowed:=ARRAY['tenant_id','event_type','payload','delivery_target','dedup_key'];
    ELSIF privilege='SELECT' AND role_name='app_returns_service' AND t.relname='orders' THEN
     allowed:=ARRAY['tenant_id','id','total_minor','currency'];
    ELSIF privilege='SELECT' AND role_name='app_returns_service' AND t.relname='idempotency_records' THEN
     allowed:=ARRAY['tenant_id','actor_kind','actor_id','operation','idempotency_key','request_hash','claim_token','state'];
    ELSIF privilege='UPDATE' AND role_name='app_returns_service' AND t.relname='idempotency_records' THEN
     allowed:=ARRAY['state','response_status','response_body_reference','completed_at'];
    ELSIF privilege='UPDATE' AND role_name='app_fulfillment_executor' THEN
     allowed:=CASE t.relname WHEN 'shipments' THEN ARRAY['sealed','order_version']
      WHEN 'returns' THEN ARRAY['creation_response','receive_response','sealed','order_version','status','restocked','received_at','receive_key','receive_hash','refund_key','refund_hash',
       'refund_expected_version','provider_ref','payment_version','income_account_id','tax_account_id','shipping_account_id','cash_account_id',
       'ledger_transaction_id','refunded_at','income_minor','tax_minor','shipping_minor','refund_minor'] ELSE allowed END;
    END IF;
    IF has_table_privilege(role_name,t.oid,privilege) IS DISTINCT FROM expected_table
     OR has_table_privilege(role_name,t.oid,privilege||' WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'FULFILLMENT_MATRIX_TABLE % %.% %',role_name,'app',t.relname,privilege; END IF;
    IF privilege IN('SELECT','INSERT','UPDATE','REFERENCES') THEN
     FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
      IF has_column_privilege(role_name,t.oid,a.attnum,privilege) IS DISTINCT FROM(a.attname::text=ANY(allowed))
       OR has_column_privilege(role_name,t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
       RAISE EXCEPTION 'FULFILLMENT_MATRIX_COLUMN % %.% %',role_name,t.relname,a.attname,privilege; END IF;
     END LOOP;
    END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid,p.proname,p.proowner,p.prosecdef,p.proconfig,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' LOOP
  IF f.proname=ANY(names) THEN
   IF f.proowner<>'app_fulfillment_executor'::regrole OR f.prosecdef IS DISTINCT FROM(f.proname NOT IN('fulfillment_allocate','fulfillment_header_guard'))
    OR cardinality(f.proconfig) IS DISTINCT FROM 1 OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
    OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
    RAISE EXCEPTION 'FULFILLMENT_MATRIX_FUNCTION %',f.proname; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_fulfillment_executor' LOOP
    expected:=role_name='app_runtime' AND f.proname IN('fulfillment_create_shipment','returns_create','returns_receive','returns_refund_snapshot')
     OR role_name='app_returns_service' AND f.proname IN('returns_refund_snapshot','returns_prepare_refund','returns_complete_refund','returns_lock_refund','returns_fail_refund')
     OR role_name='app_payments_service' AND f.proname IN('returns_lock_refund','returns_compensate_refund');
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
     RAISE EXCEPTION 'FULFILLMENT_MATRIX_ROUTINE % %',role_name,f.proname; END IF;
   END LOOP;
  END IF;
  IF has_function_privilege('app_fulfillment_executor',f.oid,'EXECUTE') IS DISTINCT FROM
   (f.proname=ANY(names) OR f.proname IN('orders_context','orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','orders_refund_budget','inventory_restore_return','payments_allocate_refund')) THEN
   RAISE EXCEPTION 'FULFILLMENT_MATRIX_FOREIGN_EXECUTOR_FUNCTION %',f.proname; END IF;
  IF has_function_privilege('app_returns_service',f.oid,'EXECUTE') IS DISTINCT FROM
   (f.proname IN('returns_refund_snapshot','returns_prepare_refund','returns_complete_refund','returns_lock_refund','returns_fail_refund','payments_post_return_refund','payments_return_context','payments_lock_return_checkout','payments_bind_return_refund','auth_revalidate_tenant')) THEN
   RAISE EXCEPTION 'FULFILLMENT_MATRIX_FOREIGN_SERVICE_FUNCTION %',f.proname; END IF;
 END LOOP;
 IF(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'FULFILLMENT_MATRIX_FUNCTION_SET'; END IF;
 IF has_table_privilege('app_runtime','app.audit_events','UPDATE') OR has_table_privilege('app_runtime','app.audit_events','DELETE')
  OR EXISTS(SELECT FROM pg_roles audit_role WHERE pg_has_role('app_runtime',audit_role.oid,'SET') AND audit_role.rolname<>'app_runtime'
   AND(has_table_privilege(audit_role.oid,'app.audit_events','UPDATE') OR has_table_privilege(audit_role.oid,'app.audit_events','DELETE'))) THEN
  RAISE EXCEPTION 'FULFILLMENT_MATRIX_AUDIT_INHERITANCE'; END IF;
END $fulfillment_matrix$;
SELECT 'PASS: fulfillment executor exact table/column/function and isolated trusted refund service matrix' AS result;

-- Orders is the sole refund budget authority; the trusted Returns service reaches
-- it only through the closed lock wrapper. Check exact identities and every
-- application role, including future roles omitted from older fixed arrays.
DO $refund_budget_matrix$
DECLARE signature text; command oid; f record; role_name text;
 owner_role oid; allowed_roles oid[];
 signatures text[]:=ARRAY[
  'app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint)',
  'app.returns_lock_refund(uuid,text)',
  'app.returns_fail_refund(uuid,text,boolean)'];
BEGIN
 FOREACH signature IN ARRAY signatures LOOP
  command:=to_regprocedure(signature);
  IF command IS NULL THEN RAISE EXCEPTION 'REFUND_BUDGET_MATRIX_SIGNATURE %',signature; END IF;
  SELECT * INTO STRICT f FROM pg_proc WHERE oid=command;
  owner_role:=CASE f.proname WHEN 'orders_refund_budget' THEN 'app_orders_executor'::regrole
   ELSE 'app_fulfillment_executor'::regrole END;
  allowed_roles:=CASE f.proname WHEN 'orders_refund_budget' THEN
   ARRAY[owner_role,'app_fulfillment_executor'::regrole::oid,'app_payments_service'::regrole::oid]
   WHEN 'returns_lock_refund' THEN ARRAY[owner_role,'app_returns_service'::regrole::oid,'app_payments_service'::regrole::oid]
   ELSE ARRAY[owner_role,'app_returns_service'::regrole::oid] END;
  IF f.prokind<>'f' OR f.proretset OR NOT f.prosecdef OR f.proowner<>owner_role
   OR f.prorettype<>(CASE f.proname WHEN 'orders_refund_budget' THEN 'app.orders'::regtype ELSE 'jsonb'::regtype END)
   OR cardinality(f.proconfig) IS DISTINCT FROM 1
   OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
    WHERE NOT grantee=ANY(allowed_roles) OR privilege_type<>'EXECUTE'
     OR is_grantable AND grantee<>owner_role) THEN
   RAISE EXCEPTION 'REFUND_BUDGET_MATRIX_DEFINITION %',signature;
  END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   IF has_function_privilege(role_name,command,'EXECUTE') IS DISTINCT FROM(role_name::regrole::oid=ANY(allowed_roles))
    OR has_function_privilege(role_name,command,'EXECUTE WITH GRANT OPTION') IS DISTINCT FROM(role_name::regrole::oid=owner_role) THEN
    RAISE EXCEPTION 'REFUND_BUDGET_MATRIX_EXECUTE % %',role_name,signature;
   END IF;
  END LOOP;
 END LOOP;
 IF(SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace
  AND proname IN('orders_refund_budget','returns_lock_refund','returns_fail_refund'))<>cardinality(signatures) THEN
  RAISE EXCEPTION 'REFUND_BUDGET_MATRIX_ROUTINE_SET';
 END IF;
END $refund_budget_matrix$;
SELECT 'PASS: shared Orders refund budget and closed Returns lock exact signatures/owners/full-role ACL' AS result;

-- Tenant capabilities expose no runtime credential reads or arbitrary key DML.
DO $tenant_auth$
DECLARE f record; role_name text; expected boolean;
 names text[]:=ARRAY['auth_role_scopes','auth_create_api_key','auth_revoke_api_key','auth_touch_api_key','auth_revalidate_tenant'];
BEGIN
 IF (SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname=ANY(names))<>5 THEN
  RAISE EXCEPTION 'TENANT_AUTH_ROUTINES_INCOMPLETE'; END IF;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname=ANY(names) LOOP
  IF f.proowner IS DISTINCT FROM 'app_credential_writer'::regrole OR f.prosecdef IS DISTINCT FROM (f.proname<>'auth_role_scopes')
   OR cardinality(f.proconfig) IS DISTINCT FROM (CASE WHEN f.proname IN ('auth_create_api_key','auth_revoke_api_key','auth_touch_api_key') THEN 2 ELSE 1 END)
   OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
   OR cardinality(f.proconfig)=2 AND f.proconfig[2] IS DISTINCT FROM 'app.tenant_id='
   OR f.oid IS DISTINCT FROM to_regprocedure(CASE f.proname
    WHEN 'auth_role_scopes' THEN 'app.auth_role_scopes(text)'
    WHEN 'auth_create_api_key' THEN 'app.auth_create_api_key(uuid,uuid,uuid,text,bytea,text[],timestamptz)'
    WHEN 'auth_revoke_api_key' THEN 'app.auth_revoke_api_key(uuid,uuid,uuid)'
    WHEN 'auth_touch_api_key' THEN 'app.auth_touch_api_key(uuid)'
    WHEN 'auth_revalidate_tenant' THEN 'app.auth_revalidate_tenant(uuid,uuid,text,uuid,text)' END)
   OR f.prorettype IS DISTINCT FROM (CASE f.proname WHEN 'auth_role_scopes' THEN 'text[]'::regtype WHEN 'auth_create_api_key' THEN 'uuid'::regtype ELSE 'boolean'::regtype END)
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
   RAISE EXCEPTION 'TENANT_AUTH_UNSAFE_ROUTINE %',f.proname; END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   expected:=role_name='app_credential_writer'
    OR role_name='app_auth' AND f.proname IN ('auth_create_api_key','auth_revoke_api_key','auth_touch_api_key')
    OR role_name IN ('app_runtime','app_auth','app_media_service','app_returns_service','app_payments_service') AND f.proname='auth_revalidate_tenant'
    OR role_name='app_tenant_lifecycle_executor' AND f.proname IN ('auth_role_scopes','auth_revalidate_tenant');
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR role_name<>'app_credential_writer' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'TENANT_AUTH_ROUTINE_ACL % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 IF NOT EXISTS(SELECT FROM pg_policy WHERE polrelid='app.tenant_memberships'::regclass
  AND polname='credential_membership_lock' AND polcmd='w' AND polpermissive
  AND polroles=ARRAY['app_credential_writer'::regrole::oid] AND polqual IS NOT NULL AND polwithcheck IS NOT NULL
  AND pg_get_expr(polqual,polrelid)=pg_get_expr(polwithcheck,polrelid)
  AND pg_get_expr(polqual,polrelid)=(SELECT pg_get_expr(i.polwithcheck,i.polrelid) FROM pg_policy i
   WHERE i.polrelid='app.tenant_memberships'::regclass AND i.polname='credential_membership_insert')) THEN
  RAISE EXCEPTION 'TENANT_AUTH_MEMBERSHIP_LOCK_POLICY'; END IF;
 IF has_table_privilege('app_auth','app.api_keys','INSERT') OR has_any_column_privilege('app_auth','app.api_keys','INSERT')
  OR has_table_privilege('app_auth','app.api_keys','UPDATE') OR has_any_column_privilege('app_auth','app.api_keys','UPDATE')
  OR has_table_privilege('app_auth','app.api_keys','DELETE')
  OR has_any_column_privilege('app_runtime','app.api_keys','SELECT') THEN RAISE EXCEPTION 'TENANT_AUTH_KEY_DML_OR_SECRET_READ'; END IF;
END $tenant_auth$;
SELECT 'PASS: tenant API-key execute-only and transaction authority capability matrix' AS result;


-- Pricing has an unreachable writer; runtime receives only two write capabilities.
DO $pricing_matrix$
DECLARE r record; t record; a record; p text; expected boolean; col boolean; f record; role_name text;
 names text[]:=ARRAY['pricing_currency','pricing_write_list','pricing_write_entry'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_pricing_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 OR NOT has_schema_privilege(r.oid,'app','USAGE') OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'PRICING_MATRIX_ROLE'; END IF;
 FOR t IN SELECT oid,relname FROM pg_class WHERE relnamespace='app'::regnamespace AND relkind IN('r','p') LOOP
  FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=t.relname IN('price_lists','price_entries') AND (p IN('SELECT','INSERT','UPDATE') OR t.relname='price_entries' AND p='DELETE');
   IF has_table_privilege(r.oid,t.oid,p) IS DISTINCT FROM expected OR has_table_privilege(r.oid,t.oid,p||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'PRICING_MATRIX_TABLE % %',t.relname,p; END IF;
   IF p IN('SELECT','INSERT','UPDATE','REFERENCES') THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     col:=expected OR t.relname='tenants' AND (p='SELECT' AND a.attname IN('id','settlement_currency') OR p='UPDATE' AND a.attname='settlement_currency')
      OR t.relname='product_variants' AND p='SELECT' AND a.attname IN('tenant_id','id');
     IF has_column_privilege(r.oid,t.oid,a.attnum,p) IS DISTINCT FROM col OR has_column_privilege(r.oid,t.oid,a.attnum,p||' WITH GRANT OPTION') THEN RAISE EXCEPTION 'PRICING_MATRIX_COLUMN % % %',t.relname,a.attname,p; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 IF (SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname=ANY(names))<>cardinality(names) THEN RAISE EXCEPTION 'PRICING_MATRIX_ROUTINES'; END IF;
 -- FOR SHARE needs UPDATE on a column even though the lock does not write it.
 -- Only pricing_currency may mention tenants: pin its reviewed SELECT-only body;
 -- all other executor routines must neither reference tenants nor execute dynamic SQL.
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  IF f.proowner=r.oid AND (
   f.proname='pricing_currency' AND encode(sha256(convert_to(f.prosrc,'UTF8')),'hex')<>'9568ab82de3c18bc96126123b48a69de90a62a46609c60c8e984aa6ecec73a33'
   OR f.proname<>'pricing_currency' AND f.prosrc ~* '\m(tenants|execute)\M') THEN
   RAISE EXCEPTION 'PRICING_EXECUTOR_TENANT_WRITE %',f.proname; END IF;
  IF f.proname=ANY(names) THEN
   IF f.proowner<>r.oid OR f.prosecdef IS DISTINCT FROM (f.proname<>'pricing_currency') OR cardinality(f.proconfig) IS DISTINCT FROM 1
    OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
    OR f.oid IS DISTINCT FROM to_regprocedure(CASE f.proname WHEN 'pricing_currency' THEN 'app.pricing_currency()'
     WHEN 'pricing_write_list' THEN 'app.pricing_write_list(uuid,bigint,jsonb)'
     WHEN 'pricing_write_entry' THEN 'app.pricing_write_entry(uuid,uuid,bigint,bigint,bigint)' END)
    OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN RAISE EXCEPTION 'PRICING_MATRIX_FUNCTION'; END IF;
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
    expected:=role_name='app_pricing_executor' OR role_name='app_runtime' AND f.proname<>'pricing_currency';
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected OR role_name<>'app_pricing_executor' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN RAISE EXCEPTION 'PRICING_MATRIX_EXECUTE % %',role_name,f.proname; END IF;
   END LOOP;
  ELSIF has_function_privilege(r.oid,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'PRICING_MATRIX_UNRELATED_FUNCTION %',f.proname;
  END IF;
 END LOOP;
END $pricing_matrix$;
SELECT 'PASS: pricing exact executor table/column/routine matrix';

-- Catalog content/listing reuse the existing definer; add no runtime grants.
DO $content_matrix$
DECLARE n int;
BEGIN
 SELECT count(*) INTO n FROM pg_trigger
 WHERE tgrelid IN ('app.product_content_revisions'::regclass,'app.product_listings'::regclass)
 AND tgname='storefront_serving_invalidate' AND NOT tgisinternal
 AND tgfoid='app.cms_serving_invalidate()'::regprocedure AND tgenabled='O';
 IF n<>2 THEN RAISE EXCEPTION 'CONTENT_INVALIDATION trigger set'; END IF;
 IF has_function_privilege('app_runtime','app.cms_serving_invalidate()','EXECUTE')
 OR has_table_privilege('app_runtime','app.serving_invalidations','INSERT') THEN
  RAISE EXCEPTION 'CONTENT_INVALIDATION runtime authority expanded';
 END IF;
END $content_matrix$;

-- Listing version guard is invoker-only and callable only through its trigger.
DO $listing_guard$ DECLARE f record; role_name text; BEGIN
 SELECT p.* INTO STRICT f FROM pg_proc p WHERE p.oid='app.catalog_listing_version()'::regprocedure;
 IF f.prokind<>'f' OR f.pronargs<>0 OR f.prorettype<>'trigger'::regtype OR f.prosecdef
  OR f.proowner<>(SELECT relowner FROM pg_class WHERE oid='app.product_listings'::regclass)
  OR cardinality(f.proconfig) IS DISTINCT FROM 1
  OR replace(f.proconfig[1],' ','') IS DISTINCT FROM 'search_path=pg_catalog,app,pg_temp'
  OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
   WHERE grantee<>f.proowner OR privilege_type<>'EXECUTE' OR is_grantable) THEN
  RAISE EXCEPTION 'CATALOG_LISTING_UNSAFE_VERSION_GUARD';
 END IF;
 FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  IF has_function_privilege(role_name,f.oid,'EXECUTE')
   OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
   RAISE EXCEPTION 'CATALOG_LISTING_GUARD_EXECUTE_MISMATCH: %',role_name;
  END IF;
 END LOOP;
END $listing_guard$;

-- Provisioning is out-of-band. Audit a representative LOGIN against the exact
-- app_returns_service matrix above; real provisioned identities run in the drill.
BEGIN;
CREATE ROLE matrix_returns_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
GRANT app_returns_service TO matrix_returns_login;
DO $coordinator$
DECLARE t record; a record; f record; p text;
BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='matrix_returns_login' AND rolcanlogin AND rolinherit
   AND NOT(rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication))
 OR EXISTS(SELECT FROM pg_roles WHERE rolname='app_returns_service' AND (rolcanlogin OR rolinherit OR rolbypassrls))
 OR (SELECT count(*) FROM pg_auth_members WHERE member='matrix_returns_login'::regrole)<>1
 OR NOT EXISTS(SELECT FROM pg_auth_members WHERE member='matrix_returns_login'::regrole
  AND roleid='app_returns_service'::regrole AND set_option AND inherit_option AND NOT admin_option) THEN
  RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_ROLE'; END IF;
 FOR t IN SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname IN('app','queue') AND c.relkind IN('r','p') LOOP
  FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   IF has_table_privilege('matrix_returns_login',t.oid,p) IS DISTINCT FROM has_table_privilege('app_returns_service',t.oid,p) THEN
    RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_TABLE'; END IF;
  END LOOP;
  FOR a IN SELECT attnum FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
   FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','REFERENCES'] LOOP
    IF has_column_privilege('matrix_returns_login',t.oid,a.attnum,p) IS DISTINCT FROM has_column_privilege('app_returns_service',t.oid,a.attnum,p) THEN
     RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_COLUMN'; END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR t IN SELECT oid FROM pg_namespace WHERE nspname IN('app','queue','public') LOOP
  FOREACH p IN ARRAY ARRAY['USAGE','CREATE'] LOOP
   IF has_schema_privilege('matrix_returns_login',t.oid,p) IS DISTINCT FROM has_schema_privilege('app_returns_service',t.oid,p)
    OR has_schema_privilege('matrix_returns_login',t.oid,p||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_SCHEMA'; END IF;
  END LOOP;
 END LOOP;
 FOR t IN SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname IN('app','queue','public') AND c.relkind='S' LOOP
  FOREACH p IN ARRAY ARRAY['USAGE','SELECT','UPDATE'] LOOP
   IF has_sequence_privilege('matrix_returns_login',t.oid,p) IS DISTINCT FROM has_sequence_privilege('app_returns_service',t.oid,p)
    OR has_sequence_privilege('matrix_returns_login',t.oid,p||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_SEQUENCE'; END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.oid FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN('app','queue') LOOP
  IF has_function_privilege('matrix_returns_login',f.oid,'EXECUTE') IS DISTINCT FROM has_function_privilege('app_returns_service',f.oid,'EXECUTE') THEN
   RAISE EXCEPTION 'RETURNS_LOGIN_MATRIX_FUNCTION'; END IF;
 END LOOP;
END $coordinator$;
ROLLBACK;

-- New Payments commands are closed to one caller capability, with no raw DML.
DO $payment_return_matrix$
DECLARE f record; role_name text; expected boolean; col text; privilege text;
BEGIN
 IF NOT EXISTS(SELECT FROM pg_class WHERE oid='app.payment_return_refunds'::regclass AND relrowsecurity AND relforcerowsecurity) THEN
  RAISE EXCEPTION 'PAYMENT_RETURN_FORCE_RLS'; END IF;
 -- Original provider authority remains private and immutable through every
 -- service role, including the Returns coordinator which calls the binder.
 FOREACH col IN ARRAY ARRAY['provider','provider_account_id','environment'] LOOP
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','REFERENCES'] LOOP
    IF has_column_privilege(role_name,'app.payment_return_refunds',col,privilege)
      IS DISTINCT FROM (role_name='app_payments_service' AND privilege='SELECT') THEN
     RAISE EXCEPTION 'PAYMENT_RETURN_BINDING_ACL % % %',role_name,col,privilege; END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT p.*,v.signature FROM (VALUES('payments_allocate_refund(uuid,jsonb,bigint)'),('payments_return_context(uuid,text)'),('payments_lock_return_checkout(uuid)'),
  ('payments_bind_return_refund(uuid,text,text,uuid)'),('payments_match_return_refund(uuid,text,text,bigint,bigint,text)')) v(signature)
 LEFT JOIN pg_proc p ON p.oid=to_regprocedure('app.'||v.signature) LOOP
  IF f.oid IS NULL OR NOT f.prosecdef OR f.proowner<>(SELECT relowner FROM pg_class WHERE oid='app.payment_return_refunds'::regclass)
   OR f.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, app, pg_temp']::text[]
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0) THEN
   RAISE EXCEPTION 'PAYMENT_RETURN_ROUTINE %',f.signature; END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   expected:=CASE WHEN f.proname='payments_allocate_refund' THEN role_name IN('app_runtime','app_fulfillment_executor') WHEN f.proname='payments_match_return_refund' THEN role_name='app_runtime' ELSE role_name='app_returns_service' OR role_name='app_payments_service' AND f.proname IN('payments_lock_return_checkout','payments_bind_return_refund') END;
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'PAYMENT_RETURN_ROUTINE_ACL % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
END $payment_return_matrix$;

-- Compensation is an unreachable Payments capability, never generic Ledger SQL.
DO $compensation_matrix$
DECLARE r record; t record; a record; f record; role_name text; privilege text;
 expected boolean; allowed text[];
 names text[]:=ARRAY['payments_finalize_refund_core','payments_guard_refund_case','payments_retain_refund_case','payments_refund_case_evidence','payments_replay_refund_case','payments_resolve_refund_case','payments_open_refund_cases',
   'payments_post_refund_compensation','payments_guard_compensation_source',
  'payments_assert_compensation_journal','payments_guard_refund_reversal','payments_finalize_refund','payments_refund_terminal','payments_post_return_refund'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_payments_service';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid)
 OR NOT has_schema_privilege(r.rolname,'app','USAGE') OR has_schema_privilege(r.rolname,'app','CREATE')
 OR has_schema_privilege(r.rolname,'app','USAGE WITH GRANT OPTION') THEN RAISE EXCEPTION 'C19D_MATRIX unsafe role'; END IF;
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=privilege='SELECT' AND t.relname=ANY(ARRAY['ledger_accounts','ledger_transactions','ledger_entries','payment_refund_attempts','payment_refund_reversals','payment_refund_terminals','payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions',
    'payment_events','payment_return_refunds','checkout_sessions','orders','order_lines','shipment_lines','webhook_inbox','returns'])
    OR privilege='INSERT' AND t.relname IN('payment_refund_attempts','payment_refund_reversals','payment_refund_terminals','payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions');
   allowed:=ARRAY[]::text[];
   IF expected THEN
    SELECT array_agg(attname::text) INTO allowed FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   ELSIF privilege='INSERT' THEN
    allowed:=CASE t.relname WHEN 'ledger_transactions' THEN ARRAY['tenant_id','id','currency','source_type','source_id','event']
     WHEN 'ledger_entries' THEN ARRAY['tenant_id','transaction_id','line_no','account_id','currency','side','amount']
     WHEN 'webhook_inbox' THEN ARRAY['tenant_id','id','provider','provider_account_id','environment','provider_event_id','payload_hash','payload_reference'] ELSE allowed END;
   ELSIF privilege='UPDATE' THEN allowed:=CASE t.relname WHEN 'ledger_transactions' THEN ARRAY['status','posted_at']
    WHEN 'checkout_sessions' THEN ARRAY['refunded_net_minor','refunded_tax_minor','refunded_shipping_minor','updated_at']
    WHEN 'webhook_inbox' THEN ARRAY['status','attempts','processed_at'] ELSE allowed END; END IF;
   IF has_table_privilege('app_payments_service',t.oid,privilege) IS DISTINCT FROM expected
    OR has_table_privilege('app_payments_service',t.oid,privilege||' WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'COMPENSATION_MATRIX_TABLE % %',t.relname,privilege; END IF;
   IF privilege=ANY(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) THEN
    FOR a IN SELECT attnum,attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege('app_payments_service',t.oid,a.attnum,privilege) IS DISTINCT FROM (a.attname::text=ANY(allowed))
      OR has_column_privilege('app_payments_service',t.oid,a.attnum,privilege||' WITH GRANT OPTION') THEN
      RAISE EXCEPTION 'COMPENSATION_MATRIX_COLUMN %.% %',t.relname,a.attname,privilege; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  IF f.proname=ANY(names) AND (f.proowner<>'app_payments_service'::regrole
   OR f.prosecdef IS DISTINCT FROM (f.proname<>'payments_guard_compensation_source')
   OR f.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, app, pg_temp']::text[]
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0)) THEN
   RAISE EXCEPTION 'COMPENSATION_MATRIX_DEFINITION %',f.proname; END IF;
  IF has_function_privilege('app_payments_service',f.oid,'EXECUTE') IS DISTINCT FROM (f.proname=ANY(names) OR f.proname IN('ledger_post_core','orders_refund_budget','orders_compensate_refund','returns_compensate_refund','returns_lock_refund',
    'payments_lock_return_checkout','payments_bind_return_refund','auth_revalidate_tenant','resolve_fake_payment_account','resolve_stripe_payment_account','lock_payment_stream','admit_payment_inbox','finish_payment_inbox')) THEN
   RAISE EXCEPTION 'COMPENSATION_MATRIX_FOREIGN_FUNCTION %',f.proname; END IF;
  IF f.proname=ANY(names) OR f.proname='ledger_post_core' THEN
   FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
    expected:=role_name='app_payments_service' OR (role_name='app_ledger_executor' AND f.proname='ledger_post_core')
     OR role_name='app_payments_ingress' AND f.proname IN('payments_finalize_refund','payments_retain_refund_case','payments_refund_case_evidence','payments_replay_refund_case','payments_resolve_refund_case')
     OR role_name='app_runtime' AND f.proname IN('payments_refund_terminal','payments_retain_refund_case','payments_open_refund_cases')
     OR role_name='app_returns_service' AND f.proname='payments_post_return_refund';
    IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
     OR has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') IS DISTINCT FROM (role_name=f.proowner::regrole::text) THEN
     RAISE EXCEPTION 'COMPENSATION_MATRIX_EXECUTE % %',role_name,f.proname; END IF;
   END LOOP;
  END IF;
 END LOOP;
 IF(SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname=ANY(names))<>cardinality(names) THEN
  RAISE EXCEPTION 'COMPENSATION_MATRIX_FUNCTION_SET'; END IF;
END $compensation_matrix$;
SELECT 'PASS: exact compensation capability and private posting core' AS result;

-- Tenant lifecycle is a separate, unassumable capability family. Exhaustive
-- table/column/function matrices prevent a new app role inheriting admission.
DO $lifecycle_acl$
DECLARE r record; f record; t record; a record; privilege text;
 expected boolean; allowed text[]; role_name text;
 names text[]:=ARRAY['tenant_lifecycle_initialize','tenant_lifecycle_read_fence','tenant_lifecycle_write_fence','tenant_lifecycle_drain_fence','tenant_lifecycle_transition_fence','tenant_lifecycle_transition','tenant_lifecycle_checkpoint'];
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_tenant_lifecycle_executor';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
  OR EXISTS(SELECT FROM pg_auth_members WHERE roleid=r.oid OR member=r.oid)
  OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'MDEL01_UNSAFE_EXECUTOR'; END IF;
 IF NOT EXISTS(SELECT FROM pg_policy WHERE polrelid='app.tenant_memberships'::regclass
  AND polname='lifecycle_membership_read' AND polcmd='r' AND polpermissive
  AND polroles=ARRAY[r.oid] AND polwithcheck IS NULL
  AND pg_get_expr(polqual,polrelid)=(SELECT pg_get_expr(p.polqual,p.polrelid) FROM pg_policy p
   WHERE p.polrelid='app.tenant_lifecycles'::regclass AND p.polname='tenant_scope')) THEN
  RAISE EXCEPTION 'MDEL01_MEMBERSHIP_READ_POLICY'; END IF;
 IF (SELECT count(*) FROM pg_proc WHERE pronamespace='app'::regnamespace AND starts_with(proname,'tenant_lifecycle_'))<>cardinality(names) THEN
  RAISE EXCEPTION 'MDEL01_ROUTINE_INVENTORY'; END IF;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname=ANY(names) LOOP
  IF f.proowner<>r.oid OR NOT f.prosecdef OR cardinality(f.proconfig)<>1
   OR replace(f.proconfig[1],' ','')<>'search_path=pg_catalog,app,pg_temp'
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0 AND privilege_type='EXECUTE') THEN
   RAISE EXCEPTION 'MDEL01_UNSAFE_ROUTINE %',f.proname; END IF;
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   expected:=role_name='app_tenant_lifecycle_executor'
    OR role_name IN ('app_runtime','app_auth','app_credential_writer') AND f.proname='tenant_lifecycle_read_fence'
    OR role_name IN ('app_runtime','app_credential_writer') AND f.proname='tenant_lifecycle_write_fence';
   IF has_function_privilege(role_name,f.oid,'EXECUTE') IS DISTINCT FROM expected
    OR role_name<>'app_tenant_lifecycle_executor' AND has_function_privilege(role_name,f.oid,'EXECUTE WITH GRANT OPTION') THEN
    RAISE EXCEPTION 'MDEL01_ROUTINE_ACL % %',role_name,f.proname; END IF;
  END LOOP;
 END LOOP;
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN ('r','p') LOOP
  FOREACH privilege IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   expected:=t.relname='tenant_lifecycles' AND privilege IN ('SELECT','INSERT','UPDATE')
    OR t.relname IN ('tenant_lifecycle_receipts','tenant_lifecycle_events') AND privilege IN ('SELECT','INSERT');
   IF has_table_privilege(r.oid,t.oid,privilege) IS DISTINCT FROM expected THEN
    RAISE EXCEPTION 'MDEL01_EXECUTOR_TABLE_ACL % %',t.relname,privilege; END IF;
   FOR a IN SELECT attname FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped LOOP
    allowed:=CASE WHEN privilege='SELECT' AND t.relname='tenants' THEN ARRAY['id','status']
     WHEN privilege='SELECT' AND t.relname='tenant_memberships' THEN ARRAY['tenant_id','user_id','role','status'] ELSE ARRAY[]::text[] END;
    IF privilege IN ('SELECT','INSERT','UPDATE','REFERENCES') AND
     has_column_privilege(r.oid,t.oid,a.attname,privilege) IS DISTINCT FROM (expected OR a.attname=ANY(allowed)) THEN
     RAISE EXCEPTION 'MDEL01_EXECUTOR_COLUMN_ACL % % %',t.relname,a.attname,privilege; END IF;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT oid,proname FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  IF has_function_privilege(r.oid,f.oid,'EXECUTE') IS DISTINCT FROM
   (f.proname=ANY(names) OR f.proname IN ('auth_role_scopes','auth_revalidate_tenant')) THEN
   RAISE EXCEPTION 'MDEL01_EXECUTOR_FOREIGN_ROUTINE %',f.proname; END IF;
 END LOOP;
END $lifecycle_acl$;
