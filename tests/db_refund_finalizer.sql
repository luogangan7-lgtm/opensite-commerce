-- Responsibility: exact closed terminal ingress and malformed-fact refusal.
-- Dependencies: all forward migrations; caller: CI check_pg, never live traffic.
-- Invariants: no ingress DML, generic posting or projection command leakage.
\set ON_ERROR_STOP on
BEGIN;
DO $gate$
DECLARE r record; f record; c record; a record; p text; expected boolean;
BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_payments_ingress';
 IF r.rolcanlogin OR r.rolinherit OR r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid)
 OR has_schema_privilege(r.oid,'app','CREATE') THEN RAISE EXCEPTION 'C19D2_INGRESS_ROLE'; END IF;
 FOR c IN SELECT oid FROM pg_class WHERE relnamespace IN('app'::regnamespace,'queue'::regnamespace) AND relkind IN('r','p') LOOP
  FOREACH p IN ARRAY ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER','MAINTAIN'] LOOP
   IF has_table_privilege(r.oid,c.oid,p) THEN RAISE EXCEPTION 'C19D2_INGRESS_DML'; END IF;
   IF p IN('SELECT','INSERT','UPDATE','REFERENCES') THEN
    FOR a IN SELECT attnum FROM pg_attribute WHERE attrelid=c.oid AND attnum>0 AND NOT attisdropped LOOP
     IF has_column_privilege(r.oid,c.oid,a.attnum,p) THEN RAISE EXCEPTION 'C19D2_INGRESS_COLUMN'; END IF;
    END LOOP;
   END IF;
  END LOOP;
 END LOOP;
 FOR f IN SELECT * FROM pg_proc WHERE pronamespace='app'::regnamespace LOOP
  expected:=f.proname IN('payments_finalize_refund','resolve_fake_payment_account','resolve_stripe_payment_account','payments_retain_refund_case','payments_refund_case_evidence','payments_replay_refund_case','payments_resolve_refund_case');
  IF has_function_privilege(r.oid,f.oid,'EXECUTE') IS DISTINCT FROM expected
   OR has_function_privilege(r.oid,f.oid,'EXECUTE WITH GRANT OPTION') THEN RAISE EXCEPTION 'C19D2_INGRESS_FUNCTION %',f.proname; END IF;
 END LOOP;
 EXECUTE 'SET LOCAL ROLE app_payments_ingress';
 BEGIN
  PERFORM app.payments_finalize_refund('{}'::jsonb);
  RAISE EXCEPTION 'C19D2_NULL_FACT_ACCEPTED';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
END $gate$;
ROLLBACK;
SELECT 'PASS C19D2 exact terminal ingress and malformed fact guard' AS result;
