-- Responsibility: preparatory compensation schema and exact inert capability gate.
-- Dependencies: all forward migrations; caller: CI check_pg only.
-- Invariant: no runtime or Returns path can mutate new financial witnesses.
\set ON_ERROR_STOP on
BEGIN;
DO $gate$
DECLARE tab text; r record; c record; p text; a record; f record;
BEGIN
 FOREACH tab IN ARRAY ARRAY['payment_refund_attempts','payment_refund_reversals'] LOOP
  IF NOT EXISTS(SELECT FROM pg_class WHERE oid=('app.'||tab)::regclass AND relrowsecurity AND relforcerowsecurity) THEN
   RAISE EXCEPTION 'C19D_GATE FORCE RLS %',tab; END IF;
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
    IF has_table_privilege(r.rolname,'app.'||tab,p) THEN RAISE EXCEPTION 'C19D_GATE table ACL % % %',r.rolname,tab,p; END IF;
   END LOOP;
   FOR a IN SELECT attnum FROM pg_attribute WHERE attrelid=('app.'||tab)::regclass AND attnum>0 AND NOT attisdropped LOOP
    FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','REFERENCES'] LOOP
     IF has_column_privilege(r.rolname,'app.'||tab,a.attnum,p) THEN RAISE EXCEPTION 'C19D_GATE column ACL % % %',r.rolname,tab,p; END IF;
    END LOOP;
   END LOOP;
  END LOOP;
 END LOOP;
 FOR f IN SELECT * FROM pg_proc WHERE oid IN('app.payments_guard_refund_attempt()'::regprocedure,'app.payments_guard_refund_reversal()'::regprocedure) LOOP
  IF f.prosecdef OR f.prorettype<>'trigger'::regtype OR f.proowner<>(SELECT relowner FROM pg_class WHERE oid='app.payment_refund_attempts'::regclass)
   OR f.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, app, pg_temp']::text[]
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) WHERE grantee=0) THEN
   RAISE EXCEPTION 'C19D_GATE guard definition'; END IF;
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   IF has_function_privilege(r.rolname,f.oid,'EXECUTE') THEN RAISE EXCEPTION 'C19D_GATE guard ACL'; END IF;
  END LOOP;
 END LOOP;
 SELECT * INTO STRICT c FROM pg_roles WHERE rolname='app_payments_service';
 IF c.rolcanlogin OR c.rolsuper OR c.rolbypassrls OR c.rolcreatedb OR c.rolcreaterole OR c.rolreplication OR c.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=c.oid OR roleid=c.oid)
 OR has_schema_privilege(c.rolname,'app','CREATE') THEN RAISE EXCEPTION 'C19D_GATE unsafe capability'; END IF;
 -- Capability is deliberately unprovisioned until the atomic finalizer lands.
 FOR r IN SELECT oid FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname IN('post_ledger','payments_guard_refund_attempt','payments_guard_refund_reversal') LOOP
  IF has_function_privilege(c.rolname,r.oid,'EXECUTE') THEN RAISE EXCEPTION 'C19D_GATE premature posting'; END IF;
 END LOOP;
END $gate$;
ROLLBACK;
SELECT 'C19D_GATE preparatory ACL PASS' AS result;
