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
DECLARE t uuid:=uuidv7(); other_t uuid:=uuidv7(); oid uuid:=uuidv7(); foreign_order uuid:=uuidv7(); guest uuid:=uuidv7();
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'customer-auth'),(other_t,'customer-auth-other');
 PERFORM set_config('test.customer_auth.tenant',t::text,true);
 PERFORM set_config('test.customer_auth.other',other_t::text,true);
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.customers(tenant_id,id,email) VALUES(t,guest,'owner@example.invalid');
 PERFORM set_config('test.customer_auth.guest',guest::text,true);
 EXECUTE 'SET LOCAL ROLE app_orders_executor';
 INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
 VALUES(t,oid,1,'customer-auth-guest',sha256('guest'::bytea),'{"email":" OWNER@example.invalid ","name":"Original"}','USD',0,0,0,'{}',0);
 UPDATE app.orders SET sealed=true WHERE tenant_id=t AND id=oid;
 EXECUTE 'RESET ROLE';
 PERFORM set_config('app.tenant_id',other_t::text,true);
 EXECUTE 'SET LOCAL ROLE app_orders_executor';
 INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
 VALUES(other_t,foreign_order,1,'customer-auth-foreign',sha256('foreign'::bytea),'{"email":"owner@example.invalid"}','USD',0,0,0,'{}',0);
 UPDATE app.orders SET sealed=true WHERE tenant_id=other_t AND id=foreign_order;
 EXECUTE 'RESET ROLE';
 PERFORM set_config('app.tenant_id',t::text,true);
 PERFORM set_config('test.customer_auth.order',oid::text,true);
 PERFORM set_config('test.customer_auth.order_snapshot',(SELECT to_jsonb(o)::text FROM app.orders o WHERE o.tenant_id=t AND o.id=oid),true);
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
 IF customer<>current_setting('test.customer_auth.guest')::uuid THEN RAISE EXCEPTION 'CUSTOMER_AUTH_GUEST_IDENTITY_REPLACED'; END IF;
 IF EXISTS(SELECT FROM app.customer_credentials WHERE tenant_id=t AND customer_id=customer)
  OR EXISTS(SELECT FROM app.customers WHERE tenant_id=t AND id=customer AND verified_at IS NOT NULL) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_CREDENTIAL_BEFORE_PROOF';
 END IF;
 -- Reset and verify challenges cannot be exchanged.
 IF app.customer_consume_reset(first_hash,phc) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_PURPOSE_BYPASS'; END IF;
 IF NOT app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_VERIFY_FIRST_LOST'; END IF;
 IF app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_VERIFY_REPLAY'; END IF;
 IF (SELECT count(*) FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id=customer)<>1
  OR NOT EXISTS(SELECT FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id=customer AND order_id=current_setting('test.customer_auth.order')::uuid) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_GUEST_ASSOCIATION'; END IF;
 IF NOT EXISTS(SELECT FROM app.customers c JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
  WHERE c.tenant_id=t AND c.id=customer AND c.verified_at IS NOT NULL AND p.password_hash=phc) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_PROOF_NOT_ATOMIC';
 END IF;
 sid:=(app.customer_save_session(customer,phc,NULL,session_hash,
  clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 hour')->>'session_id')::uuid;
 IF sid IS NULL OR app.customer_session(session_hash,3600) IS NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_SESSION_MISSING'; END IF;
 IF NOT app.customer_prepare_reset('owner@example.invalid',reset_hash,clock_timestamp()+interval '1 hour') THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_MISSING';
 END IF;
 IF app.customer_consume_verification(reset_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_PURPOSE_BYPASS'; END IF;
 IF NOT app.customer_consume_reset(reset_hash,replace(phc,'YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU','ZWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU')) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_FIRST_LOST';
 END IF;
 IF app.customer_consume_reset(reset_hash,phc) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_REPLAY'; END IF;
 IF app.customer_session(session_hash,3600) IS NOT NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_RESET_SESSION_SURVIVED'; END IF;
 IF app.customer_save_session(customer,phc,NULL,sha256('stale-login'::bytea),clock_timestamp()+interval '1 day',clock_timestamp()+interval '1 hour') IS NOT NULL THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_STALE_KDF_SESSION'; END IF;
 PERFORM set_config('app.tenant_id',current_setting('test.customer_auth.other'),true);
 SELECT count(*) INTO n FROM app.customer_auth_tokens;
 IF n<>0 OR app.customer_consume_verification(first_hash) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_CROSS_TENANT'; END IF;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM app.customer_sessions) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_BLANK_TENANT'; END IF;
END $proof$;
RESET ROLE;
DO $snapshot$ BEGIN
 IF (SELECT to_jsonb(o)::text FROM app.orders o WHERE o.tenant_id=current_setting('test.customer_auth.tenant')::uuid AND o.id=current_setting('test.customer_auth.order')::uuid)
  IS DISTINCT FROM current_setting('test.customer_auth.order_snapshot') THEN RAISE EXCEPTION 'CUSTOMER_AUTH_ORDER_SNAPSHOT_CHANGED'; END IF;
END $snapshot$;
SET LOCAL ROLE app_customer_auth;
DO $limits$
DECLARE t uuid:=current_setting('test.customer_auth.tenant')::uuid; delay bigint; n integer; before_cooldown timestamptz;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 FOR n IN 1..11 LOOP
  delay:=app.customer_admit(sha256('fixed-account'::bytea),sha256(convert_to('rotating-ip-'||n,'UTF8')),clock_timestamp());
 END LOOP;
 IF delay<=0 OR delay>60 THEN RAISE EXCEPTION 'CUSTOMER_AUTH_ACCOUNT_DISTRIBUTED_LIMIT'; END IF;
 -- Limited caller cannot directly advance verification or write any secret.
 BEGIN UPDATE app.customers SET verified_at=clock_timestamp() WHERE tenant_id=t;
  RAISE EXCEPTION 'CUSTOMER_AUTH_VERIFIED_DIRECT_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.customer_auth_tokens SET consumed_at=clock_timestamp() WHERE tenant_id=t;
  RAISE EXCEPTION 'CUSTOMER_AUTH_TOKEN_DIRECT_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id)
  VALUES(t,current_setting('test.customer_auth.guest')::uuid,current_setting('test.customer_auth.order')::uuid);
  RAISE EXCEPTION 'CUSTOMER_AUTH_LINK_DIRECT_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM guest_contact FROM app.orders;
  RAISE EXCEPTION 'CUSTOMER_AUTH_ORDER_CONTACT_DIRECT_READ'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $limits$;
RESET ROLE;

-- Owner-controlled clock fixtures exercise expiration without a sleep or weakened CHECK.
DO $lifecycle$
DECLARE t uuid:=current_setting('test.customer_auth.tenant')::uuid; cid uuid; phc text; first jsonb; refreshed jsonb;
 expires_token bytea:=sha256('expired-proof'::bytea); rollback_token bytea:=sha256('rollback-proof'::bytea);
 old_session bytea:=sha256('rotate-source'::bytea); new_session bytea:=sha256('rotate-target'::bytea);
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 SELECT customer_id,password_hash INTO STRICT cid,phc FROM app.customer_credentials WHERE tenant_id=t;
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 PERFORM app.customer_prepare_registration('expiry@example.invalid',phc,expires_token,clock_timestamp()+interval '1 hour');
 PERFORM app.customer_prepare_registration('rollback@example.invalid',phc,rollback_token,clock_timestamp()+interval '1 hour');
 -- A transaction failure after consumption rolls back token, credential and verification together.
 BEGIN
  IF NOT app.customer_consume_verification(rollback_token) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_ROLLBACK_SETUP'; END IF;
  RAISE EXCEPTION 'CUSTOMER_AUTH_INTENTIONAL_ROLLBACK' USING ERRCODE='P0002';
 EXCEPTION WHEN no_data_found THEN NULL; END;
 IF EXISTS(SELECT FROM app.customer_auth_tokens WHERE token_hash=rollback_token AND consumed_at IS NOT NULL)
 OR EXISTS(SELECT FROM app.customers c JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
 WHERE c.email_normalized='rollback@example.invalid') THEN RAISE EXCEPTION 'CUSTOMER_AUTH_PARTIAL_ROLLBACK'; END IF;
 first:=app.customer_save_session(cid,phc,NULL,old_session,clock_timestamp()+interval '5 minutes',clock_timestamp()+interval '2 minutes');
 refreshed:=app.customer_session(old_session,3600);
 IF first IS NULL OR refreshed IS NULL OR (refreshed->>'idle_expires_at')::timestamptz>(refreshed->>'expires_at')::timestamptz
 OR refreshed->>'expires_at' IS DISTINCT FROM first->>'expires_at' THEN RAISE EXCEPTION 'CUSTOMER_AUTH_ABSOLUTE_IDLE_CAP'; END IF;
 IF app.customer_save_session(cid,phc,old_session,new_session,clock_timestamp()+interval '5 minutes',clock_timestamp()+interval '2 minutes') IS NULL
 OR app.customer_session(old_session,3600) IS NOT NULL
 OR app.customer_save_session(cid,phc,old_session,sha256('rotate-replay'::bytea),clock_timestamp()+interval '5 minutes',clock_timestamp()+interval '2 minutes') IS NOT NULL THEN
 RAISE EXCEPTION 'CUSTOMER_AUTH_ROTATE_REPLAY'; END IF;
 EXECUTE 'RESET ROLE';
 UPDATE app.customer_auth_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour'
 WHERE tenant_id=t AND token_hash=expires_token;
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 IF app.customer_consume_verification(expires_token) THEN RAISE EXCEPTION 'CUSTOMER_AUTH_EXPIRED_PROOF'; END IF;
 EXECUTE 'RESET ROLE';
 UPDATE app.customers SET status='disabled' WHERE tenant_id=t AND id=cid;
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 IF app.customer_session(new_session,3600) IS NOT NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_DISABLED_CUSTOMER_SESSION'; END IF;
 EXECUTE 'RESET ROLE';
 UPDATE app.customers SET status='active' WHERE tenant_id=t AND id=cid;
 UPDATE app.tenants SET status='suspended' WHERE id=t;
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 IF app.customer_session(new_session,3600) IS NOT NULL OR app.customer_prepare_reset('owner@example.invalid',sha256('disabled-reset'::bytea),clock_timestamp()+interval '1 hour') THEN
 RAISE EXCEPTION 'CUSTOMER_AUTH_DISABLED_TENANT'; END IF;
 EXECUTE 'RESET ROLE';
 UPDATE app.tenants SET status='active' WHERE id=t;
END $lifecycle$;
DO $durable_limits$
DECLARE t uuid:=uuidv7(); n integer; delay bigint; fixed_account bytea:=sha256('cooldown-account'::bytea);
 fixed_ip bytea:=sha256('cooldown-ip'::bytea); cutoff timestamptz;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'customer-auth-durable-limits');
 PERFORM set_config('app.tenant_id',t::text,true);
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 PERFORM app.customer_admit(fixed_account,fixed_ip,clock_timestamp());
 FOR n IN 1..5 LOOP PERFORM app.customer_failed(fixed_account,fixed_ip); END LOOP;
 delay:=app.customer_admit(fixed_account,fixed_ip,clock_timestamp());
 IF delay<=0 OR delay>60 THEN RAISE EXCEPTION 'CUSTOMER_AUTH_FAILED_COOLDOWN'; END IF;
 EXECUTE 'RESET ROLE';
 SELECT cooldown_until INTO STRICT cutoff FROM app.customer_auth_limits WHERE tenant_id=t AND kind='account' AND key_hash=fixed_account;
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 PERFORM app.customer_failed(fixed_account,fixed_ip);
 EXECUTE 'RESET ROLE';
 IF (SELECT cooldown_until FROM app.customer_auth_limits WHERE tenant_id=t AND kind='account' AND key_hash=fixed_account)<>cutoff THEN
 RAISE EXCEPTION 'CUSTOMER_AUTH_COOLDOWN_EXTENDED'; END IF;
 -- Rotating accounts cannot evade one IP counter; rotating both cannot evade global.
 EXECUTE 'SET LOCAL ROLE app_customer_auth';
 FOR n IN 1..31 LOOP delay:=app.customer_admit(sha256(convert_to('ip-account-'||n,'UTF8')),sha256('fixed-ip'::bytea),clock_timestamp()); END LOOP;
 IF delay<=0 OR delay>60 THEN RAISE EXCEPTION 'CUSTOMER_AUTH_IP_DISTRIBUTED_LIMIT'; END IF;
 FOR n IN 1..301 LOOP delay:=app.customer_admit(sha256(convert_to('global-account-'||n,'UTF8')),sha256(convert_to('global-ip-'||n,'UTF8')),clock_timestamp()); END LOOP;
 IF delay<=0 OR delay>60 THEN RAISE EXCEPTION 'CUSTOMER_AUTH_GLOBAL_DISTRIBUTED_LIMIT'; END IF;
 EXECUTE 'RESET ROLE';
 IF EXISTS(SELECT FROM app.customer_auth_limits WHERE tenant_id=t AND key_hash=sha256('global-account-301'::bytea))
 OR(SELECT attempts FROM app.customer_auth_limits WHERE tenant_id=t AND kind='global')<>301 THEN
 RAISE EXCEPTION 'CUSTOMER_AUTH_GLOBAL_ALLOCATION_BOUND'; END IF;
END $durable_limits$;
ROLLBACK;
SELECT 'PASS: customer email proof, purpose, single-use, reset revocation and tenant isolation';
