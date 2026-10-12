-- 008 / Critical: run under a dedicated privileged migration role.
-- app_runtime is NOLOGIN and has NO DDL, BYPASSRLS or TRUNCATE privileges.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_runtime') THEN
  CREATE ROLE app_runtime NOLOGIN;
 END IF;
END $$;
GRANT USAGE ON SCHEMA app TO app_runtime;

-- Put FORCE RLS on EVERY tenant-scoped table; opt-out is not allowed.
DO $policies$
DECLARE r record;
BEGIN
 FOR r IN
  SELECT t.table_name
  FROM information_schema.tables t
  JOIN information_schema.columns c
   ON c.table_schema=t.table_schema AND c.table_name=t.table_name
  WHERE t.table_schema='app' AND t.table_type='BASE TABLE' AND c.column_name='tenant_id'
 LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',r.table_name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',r.table_name);
  EXECUTE format(
   'CREATE POLICY tenant_scope ON app.%I TO app_runtime USING (tenant_id = nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK (tenant_id = nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',
   r.table_name
  );
 END LOOP;
END $policies$;

-- Only already-authenticated service requests may use app_runtime.
-- Credential verification uses a distinct, tightly scoped auth database role;
-- neither sessions, password hashes nor API key hashes are readable here.
DO $grants$
DECLARE r record;
BEGIN
 FOR r IN
  SELECT t.table_name
  FROM information_schema.tables t
  JOIN information_schema.columns c
   ON c.table_schema=t.table_schema AND c.table_name=t.table_name
  WHERE t.table_schema='app' AND t.table_type='BASE TABLE' AND c.column_name='tenant_id'
 LOOP
  IF r.table_name IN ('api_keys') THEN CONTINUE; END IF;
  EXECUTE format('GRANT SELECT, INSERT ON app.%I TO app_runtime',r.table_name);
  IF r.table_name NOT IN (
    'product_content_revisions','page_revisions','blog_post_revisions',
    'company_profile_revisions','site_releases','site_release_routes',
    'site_release_templates','site_release_media','audit_events','draft_operations'
  ) THEN
    EXECUTE format('GRANT UPDATE ON app.%I TO app_runtime',r.table_name);
  END IF;
 END LOOP;
END $grants$;
-- No GRANT DELETE or TRUNCATE. Cleanup uses a separate maintenance identity.
-- Global identity tables (app.users, app.auth_sessions) have no app_runtime grants.
