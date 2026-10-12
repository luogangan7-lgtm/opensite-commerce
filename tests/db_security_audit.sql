-- Executed as a privileged migrator on an expendable PostgreSQL 18 database.
-- Check row security, grants, and deliberate separation of database roles.
DO $audit$
DECLARE bad_name text;
BEGIN
 SELECT format('%I.%I',n.nspname,c.relname) INTO bad_name
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='app' AND c.relkind='r'
   AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid
     AND a.attname='tenant_id' AND NOT a.attisdropped)
   AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity)
 LIMIT 1;
 IF bad_name IS NOT NULL THEN
   RAISE EXCEPTION 'Tenant table missing ENABLE/FORCE RLS: %', bad_name;
 END IF;
 -- RLS flags alone are not proof that the runtime can operate within its tenant.
 SELECT format('%I.%I',n.nspname,c.relname) INTO bad_name
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='app' AND c.relkind='r'
   AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid
     AND a.attname='tenant_id' AND NOT a.attisdropped)
   AND NOT EXISTS (SELECT 1 FROM pg_policies p
     WHERE p.schemaname='app' AND p.tablename=c.relname AND p.policyname='tenant_scope')
 LIMIT 1;
 IF bad_name IS NOT NULL THEN
   RAISE EXCEPTION 'Tenant table missing tenant_scope policy: %', bad_name;
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_runtime' AND NOT rolcanlogin AND NOT rolbypassrls AND NOT rolsuper) THEN
   RAISE EXCEPTION 'app_runtime does not meet role restrictions';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_auth' AND NOT rolcanlogin AND NOT rolbypassrls AND NOT rolsuper) THEN
   RAISE EXCEPTION 'app_auth role missing/unsafe';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_domain_resolver' AND NOT rolcanlogin AND NOT rolbypassrls AND NOT rolsuper) THEN
   RAISE EXCEPTION 'domain resolver role missing/unsafe';
 END IF;
 IF has_table_privilege('app_runtime','app.users','SELECT')
   OR has_table_privilege('app_runtime','app.api_keys','SELECT')
   OR has_table_privilege('app_runtime','app.auth_sessions','SELECT')
   OR has_table_privilege('app_runtime','app.products','DELETE')
   OR has_table_privilege('app_runtime','app.products','TRUNCATE')
   OR has_schema_privilege('app_runtime','app','CREATE') THEN
    RAISE EXCEPTION 'runtime privilege escalation';
 END IF;
 IF NOT has_table_privilege('app_auth','app.users','SELECT')
   OR NOT has_table_privilege('app_auth','app.api_keys','SELECT')
   OR has_table_privilege('app_auth','app.products','SELECT')
   OR has_table_privilege('app_domain_resolver','app.users','SELECT')
   OR has_table_privilege('app_domain_resolver','app.media_blobs','SELECT') THEN
    RAISE EXCEPTION 'auth/domain resolver incorrect privilege scope';
 END IF;
END $audit$;
SELECT 'PASS: database roles and FORCE RLS flags' AS result;
