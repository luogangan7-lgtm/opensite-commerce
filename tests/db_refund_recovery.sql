-- Responsibility: exact recovery ACL/RLS/append-only contract, CI disposable PG.
-- Dependencies: all forward migrations; no fixed migration numbers.
\set ON_ERROR_STOP on
DO $recovery$
DECLARE n text; t regclass; f regprocedure; role_name text;
BEGIN
 FOREACH n IN ARRAY ARRAY['payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions'] LOOP
  t:=('app.'||n)::regclass;
  IF NOT EXISTS(SELECT FROM pg_class WHERE oid=t AND relrowsecurity AND relforcerowsecurity)
   OR NOT EXISTS(SELECT FROM pg_trigger WHERE tgrelid=t AND NOT tgisinternal AND tgname='refund_case_guard') THEN
   RAISE EXCEPTION 'C19D3_RLS_AUDIT_GATE %',n; END IF;
  FOREACH role_name IN ARRAY ARRAY['app_runtime','app_returns_service','app_payments_ingress','app_auth','app_customer_auth'] LOOP
   IF has_table_privilege(role_name,t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE') THEN RAISE EXCEPTION 'C19D3_PRIVATE_CASE_GATE % %',role_name,n; END IF;
  END LOOP;
 END LOOP;
 FOREACH n IN ARRAY ARRAY['payments_replay_refund_case(uuid,jsonb)','payments_resolve_refund_case(uuid,jsonb)','payments_refund_case_evidence(text,text)'] LOOP
  f:=('app.'||n)::regprocedure;
  IF NOT has_function_privilege('app_payments_ingress',f,'EXECUTE')
   OR has_function_privilege('app_runtime',f,'EXECUTE') OR has_function_privilege('app_returns_service',f,'EXECUTE') THEN
   RAISE EXCEPTION 'C19D3_RECOVERY_CAPABILITY_GATE %',n; END IF;
 END LOOP;
 IF has_function_privilege('app_payments_ingress','app.payments_finalize_refund_core(jsonb)','EXECUTE') THEN
  RAISE EXCEPTION 'C19D3_CORE_BYPASS_GATE'; END IF;
END $recovery$;
SELECT 'PASS: C19D3 recovery capability and immutable audit gate' AS result;
