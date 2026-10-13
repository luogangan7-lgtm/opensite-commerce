-- Responsibility: customer authentication invariants on the real CI PostgreSQL.
-- Dependencies: customer_auth forward migration; called by check_pg only in CI.
-- Invariants: email proof, token purpose/single-use, tenant isolation and sessions.
BEGIN;
DO $schema$
DECLARE tab text;
BEGIN
 FOREACH tab IN ARRAY ARRAY['customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits'] LOOP
  IF to_regclass('app.'||tab) IS NULL THEN
   RAISE EXCEPTION 'CUSTOMER_AUTH_SCHEMA_MISSING: %',tab;
  END IF;
  IF NOT EXISTS(SELECT FROM pg_class WHERE oid=to_regclass('app.'||tab) AND relrowsecurity AND relforcerowsecurity) THEN
   RAISE EXCEPTION 'CUSTOMER_AUTH_FORCE_RLS_MISSING: %',tab;
  END IF;
 END LOOP;
END $schema$;
DO $fixture$
DECLARE t uuid:=uuidv7(); other_t uuid:=uuidv7();
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'customer-auth'),(other_t,'customer-auth-other');
 PERFORM set_config('test.customer_auth.tenant',t::text,true);
 PERFORM set_config('test.customer_auth.other',other_t::text,true);
 PERFORM set_config('app.tenant_id',t::text,true);
END $fixture$;
SET LOCAL ROLE app_customer_auth;
DO $proof$
DECLARE t uuid:=current_setting('test.customer_auth.tenant')::uuid;
 customer uuid; sid uuid; n integer;
 phc text:=concat('$argon2id$v=19$m=19456,t=2,p=1$',
  'YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU');
 first_hash bytea:=sha256('customer-auth-verify'::bytea);
 reset_hash bytea:=sha256('customer-auth-reset'::bytea);
 session_hash bytea:=sha256('customer-auth-session'::bytea);
BEGIN
 PERFORM app.customer_prepare_registration(' Owner@Example.invalid ',phc,first_hash,clock_timestamp()+interval '1 hour');
 SELECT id INTO STRICT customer FROM app.customers WHERE tenant_id=t AND email_normalized='owner@example.invalid';
 IF EXISTS(SELECT FROM app.customer_credentials WHERE tenant_id=t AND customer_id=customer)
  OR EXISTS(SELECT FROM app.customers WHERE tenant_id=t AND id=customer AND verified_at IS NOT NULL) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_CREDENTIAL_BEFORE_PROOF';
 END IF;
 -- Reset and verify challenges cannot be exchanged.
 IF app.customer_consume_reset(first_hash,phc) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_PURPOSE_BYPASS'; END IF;
 IF NOT app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_VERIFY_FIRST_LOST'; END IF;
 IF app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_VERIFY_REPLAY'; END IF;
 IF NOT EXISTS(SELECT FROM app.customers c JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
  WHERE c.tenant_id=t AND c.id=customer AND c.verified_at IS NOT NULL AND p.password_hash=phc) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_PROOF_NOT_ATOMIC';
 END IF;
 sid:=(app.customer_save_session(customer,NULL,session_hash,
  clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 hour')->>'id')::uuid;
 IF sid IS NULL OR app.customer_session(session_hash,3600) IS NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_SESSION_MISSING'; END IF;
 IF NOT app.customer_prepare_reset('owner@example.invalid',reset_hash,clock_timestamp()+interval '1 hour') THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_MISSING';
 END IF;
 IF app.customer_consume_verification(reset_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_PURPOSE_BYPASS'; END IF;
 IF NOT app.customer_consume_reset(reset_hash,replace(phc,'m=19456','m=19457')) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_FIRST_LOST';
 END IF;
 IF app.customer_consume_reset(reset_hash,phc) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_REPLAY'; END IF;
 IF app.customer_session(session_hash,3600) IS NOT NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_SESSION_SURVIVED'; END IF;
 PERFORM set_config('app.tenant_id',current_setting('test.customer_auth.other'),true);
 SELECT count(*) INTO n FROM app.customer_auth_tokens;
 IF n<>0 OR app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_CROSS_TENANT'; END IF;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM app.customer_sessions) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_BLANK_TENANT'; END IF;
END $proof$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: customer email proof, purpose, single-use, reset revocation and tenant isolation';
