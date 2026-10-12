-- Responsibility: typed offline bootstrap port; no Go-side ad-hoc business SQL.
-- Dependencies: identity_auth migration and PG capability catalogs.
-- Caller: private identity bootstrap store, on a dedicated offline LOGIN.
-- Invariants: reject migrator/runtime/mixed-capability credentials before writes.
-- name: BootstrapAdmin :one
SELECT app.bootstrap_admin(sqlc.arg(email)::text,sqlc.arg(tenant_name)::text,sqlc.arg(password_hash)::text)::uuid AS tenant_id;

-- name: ValidateBootstrapConnection :one
SELECT current_user=session_user
 AND EXISTS(SELECT FROM pg_catalog.pg_roles r WHERE r.rolname=current_user
  AND r.rolcanlogin AND NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication))
 AND pg_catalog.pg_has_role(current_user,'app_admin_bootstrap','USAGE')
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_roles r WHERE r.rolname NOT IN (current_user,'app_admin_bootstrap')
  AND pg_catalog.pg_has_role(current_user,r.oid,'MEMBER'))
 AND NOT pg_catalog.has_schema_privilege(current_user,'app','CREATE')
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN ('r','p') AND
   (pg_catalog.has_table_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
    OR pg_catalog.has_any_column_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')))
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.oid<>'app.bootstrap_admin(text,text,text)'::regprocedure
   AND pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE')) AS allowed;
