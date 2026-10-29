-- Responsibility: Identity merchant-deletion proof, fencing and receipt witnesses.
-- Dependencies: all forward migrations; real disposable PostgreSQL 18 CI only.
-- Invariants: tenant/session/action binding, one use, DB expiry after locks,
-- other-tenant global accounts survive, no application role can consume proofs.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL app.session_idle_seconds='1800';
DO $schema$
BEGIN
 IF to_regclass('app.identity_merchant_proofs') IS NULL THEN
  RAISE EXCEPTION 'MDEL02_SCHEMA_REQUIRED';
 END IF;
END $schema$;
INSERT INTO app.tenants(name) VALUES('mdel02-a') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('mdel02-b') RETURNING id AS tb \gset
INSERT INTO app.users(email,password_hash) VALUES('mdel02-owner@invalid.test','verified-phc') RETURNING id AS owner_id \gset
INSERT INTO app.users(email,password_hash) VALUES('mdel02-admin@invalid.test','verified-phc') RETURNING id AS admin_id \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'ta',:'owner_id','owner'),(:'tb',:'owner_id','owner'),(:'ta',:'admin_id','admin');
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'owner_id',sha256('mdel02-session'::bytea),clock_timestamp()+interval '1 day') RETURNING id AS sess \gset
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'owner_id',sha256('mdel02-other-session'::bytea),clock_timestamp()+interval '1 day') RETURNING id AS other_sess \gset
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'admin_id',sha256('mdel02-admin'::bytea),clock_timestamp()+interval '1 day') RETURNING id AS admin_sess \gset
SELECT set_config('test.mdel02.a',:'ta',true),set_config('test.mdel02.b',:'tb',true),
 set_config('test.mdel02.owner',:'owner_id',true),set_config('test.mdel02.admin',:'admin_id',true),
 set_config('test.mdel02.sess',:'sess',true),set_config('test.mdel02.other',:'other_sess',true),set_config('test.mdel02.admin_sess',:'admin_sess',true);
DO $test$
DECLARE t uuid:=current_setting('test.mdel02.a')::uuid; b uuid:=current_setting('test.mdel02.b')::uuid;
 u uuid:=current_setting('test.mdel02.owner')::uuid; s uuid:=current_setting('test.mdel02.sess')::uuid;
 d bytea:=sha256('proof-secret'::bytea); until_db timestamptz; before_db timestamptz;
 req uuid:=uuidv7(); e bigint; n bigint; key_id uuid:=uuidv7(); phase_value text; expected_count bigint; other_before bytea; other_after bytea;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 -- Ordinary write revalidation uses the configured timeout, not a SQL default.
 BEGIN
  PERFORM set_config('app.session_idle_seconds','60',true);
  WITH instant AS MATERIALIZED(SELECT clock_timestamp() at)
   UPDATE app.auth_sessions SET created_at=instant.at-interval '2 hours',last_seen_at=instant.at-interval '61 seconds' FROM instant WHERE id=s;
  PERFORM set_config('app.session_idle_seconds','120',true);
  IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:write') IS DISTINCT FROM true THEN
   RAISE EXCEPTION 'MDEL02_WRITE_CONFIGURED_IDLE'; END IF;
  PERFORM set_config('app.session_idle_seconds','60',true);
  IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:write') IS DISTINCT FROM false THEN
   RAISE EXCEPTION 'MDEL02_WRITE_IDLE_REVALIDATION'; END IF;
  RAISE EXCEPTION 'restore idle witness' USING ERRCODE='P9002';
 EXCEPTION WHEN SQLSTATE 'P9002' THEN NULL; END;

 INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,scopes) VALUES(t,key_id,u,'mdel02',sha256('mdel02-key'::bytea),ARRAY['catalog:read','tenant:delete']);
 IF app.auth_revalidate_tenant(t,u,'api_key',key_id,'tenant:delete') IS DISTINCT FROM true THEN RAISE EXCEPTION 'MDEL02_KEY_FIXTURE'; END IF;
 -- A key scope is not a session; passing a key ID as a session is denied.
 BEGIN
  PERFORM app.auth_issue_merchant_proof(t,u,key_id,'request',d,'verified-phc');
  RAISE EXCEPTION 'MDEL02_KEY_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 before_db:=clock_timestamp();
 until_db:=app.auth_issue_merchant_proof(t,u,s,'request',d,'verified-phc');
 IF until_db<before_db+interval '5 minutes' OR until_db>clock_timestamp()+interval '5 minutes' THEN
  RAISE EXCEPTION 'MDEL02_DB_EXPIRY'; END IF;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(b,u,s,'request',d);
  RAISE EXCEPTION 'MDEL02_CROSS_TENANT_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,current_setting('test.mdel02.other')::uuid,'request',d);
  RAISE EXCEPTION 'MDEL02_SESSION_BINDING';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,s,'cancel',d);
  RAISE EXCEPTION 'MDEL02_ACTION_BINDING';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM app.auth_consume_merchant_proof(t,u,s,'request',d);
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,s,'request',d);
  RAISE EXCEPTION 'MDEL02_PROOF_REPLAY';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM app.auth_issue_merchant_proof(t,u,s,'request',d,'verified-phc');
 WITH instant AS (SELECT clock_timestamp() AS at) UPDATE app.identity_merchant_proofs SET created_at=instant.at-interval '5 minutes',expires_at=instant.at FROM instant WHERE tenant_id=t;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,s,'request',d);
  RAISE EXCEPTION 'MDEL02_EXPIRED_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.auth_issue_merchant_proof(t,u,s,'request',sha256('stale'::bytea),'stale-phc');
  RAISE EXCEPTION 'MDEL02_PASSWORD_FENCE';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.auth_issue_merchant_proof(t,current_setting('test.mdel02.admin')::uuid,current_setting('test.mdel02.admin_sess')::uuid,'request',d,'verified-phc');
  RAISE EXCEPTION 'MDEL02_ADMIN_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM app.auth_issue_merchant_proof(t,u,s,'request',d,'verified-phc');
 UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE id=s;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,s,'request',d);
  RAISE EXCEPTION 'MDEL02_REVOKED_SESSION_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 UPDATE app.auth_sessions SET revoked_at=NULL WHERE id=s;
 PERFORM app.auth_issue_merchant_proof(t,u,s,'request',d,'verified-phc');
 UPDATE app.users SET password_hash='replacement-phc' WHERE id=u;
 BEGIN
  PERFORM app.auth_consume_merchant_proof(t,u,s,'request',d);
  RAISE EXCEPTION 'MDEL02_CHANGED_PASSWORD_PROOF';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 -- Reset fixture sessions after proving global password revocation.
 UPDATE app.auth_sessions SET revoked_at=NULL WHERE user_id=u;
 PERFORM app.auth_issue_merchant_proof(t,u,s,'request',d,'replacement-phc');
 -- Populate both action slots in each tenant; global identity is shared.
 PERFORM app.auth_issue_merchant_proof(t,u,s,'cancel',sha256('target-cancel'::bytea),'replacement-phc');
 PERFORM set_config('app.tenant_id',b::text,true);
 PERFORM app.auth_issue_merchant_proof(b,u,s,'request',sha256('other-request'::bytea),'replacement-phc');
 PERFORM app.auth_issue_merchant_proof(b,u,s,'cancel',sha256('other-cancel'::bytea),'replacement-phc');
 SELECT convert_to(jsonb_agg(to_jsonb(p) ORDER BY id)::text,'UTF8') INTO STRICT other_before
  FROM app.identity_merchant_proofs p WHERE tenant_id=b;
 IF (SELECT count(*) FROM app.identity_merchant_proofs WHERE tenant_id=b)<>2 THEN RAISE EXCEPTION 'MDEL02_OTHER_PROOF_FIXTURE'; END IF;
 PERFORM set_config('app.tenant_id',t::text,true);
 SELECT count(*) INTO expected_count FROM app.identity_merchant_proofs WHERE tenant_id=t;
 IF expected_count<>2 THEN RAISE EXCEPTION 'MDEL02_TARGET_PROOF_FIXTURE'; END IF;
 -- Lifecycle state is controlled by Tenant. Set a worker-admitted fixture;
 -- Identity never writes this table in production.
 PERFORM app.tenant_lifecycle_transition(t,req,u,s,'request',uuidv7(),sha256('request'::bytea),0,
  '{"paid_unfulfilled":0,"fulfillment_return":0,"payment_refund":0,"checkout_capture":0,"refund_reconciliation":0,"pending_payout":0,"unsettled_provider_balance":0,"legal_hold":0}');
 IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:write') THEN RAISE EXCEPTION 'MDEL02_COOLING_WRITE'; END IF;
 IF NOT app.auth_revalidate_tenant(t,u,'session',s,'catalog:read') THEN RAISE EXCEPTION 'MDEL02_COOLING_READ'; END IF;
 IF NOT app.auth_revalidate_tenant(t,u,'session',s,'tenant:delete') THEN RAISE EXCEPTION 'MDEL02_CANCEL_AUTHORITY'; END IF;
 -- Every pre-destructive failure phase preserves read/cancel but denies writes.
 FOREACH phase_value IN ARRAY ARRAY['none','preconditions','executing','completed'] LOOP
  UPDATE app.tenant_lifecycles SET state='failed',phase=phase_value WHERE tenant_id=t;
  IF app.auth_merchant_scope(t,'session','catalog:read') IS DISTINCT FROM (phase_value IN('none','preconditions'))
   OR app.auth_merchant_scope(t,'session','catalog:write')
   OR app.auth_merchant_scope(t,'api_key','catalog:read') THEN
   RAISE EXCEPTION 'MDEL02_FAILED_PHASE_SCOPE'; END IF;
 END LOOP;
 UPDATE app.tenant_lifecycles SET state='cooling_off',phase='executing' WHERE tenant_id=t;
 IF NOT app.auth_merchant_scope(t,'session','catalog:read') THEN RAISE EXCEPTION 'MDEL02_COOLING_PHASE_SCOPE'; END IF;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing',epoch=epoch+1 WHERE tenant_id=t RETURNING epoch INTO e;
 IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:read') OR app.auth_revalidate_tenant(t,u,'session',s,'tenant:delete')
  OR app.auth_revalidate_tenant(t,u,'api_key',key_id,'catalog:read') THEN
  RAISE EXCEPTION 'MDEL02_EXECUTION_SESSION_FENCE'; END IF;
 -- This guard belongs to Tenant and must precede Identity's interactive fence.
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,req,u,s,'cancel',uuidv7(),sha256('cancel'::bytea),e,'{}');
  RAISE EXCEPTION 'MDEL02_TENANT_CANCEL_GUARD';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL;
  WHEN insufficient_privilege THEN RAISE EXCEPTION 'MDEL02_TENANT_CANCEL_GUARD'; END;
 -- Use the actual target context so the wrong-request witness reaches the
 -- lifecycle validation, rather than vacuously failing the GUC fence.
 PERFORM set_config('app.tenant_id',b::text,true);
 BEGIN
  PERFORM app.auth_erase_merchant_proofs(b,req,e);
  RAISE EXCEPTION 'MDEL02_ERASE_REQUEST_BINDING';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 PERFORM set_config('app.tenant_id',t::text,true);
 BEGIN
  PERFORM app.auth_erase_merchant_proofs(t,req,e-1);
  RAISE EXCEPTION 'MDEL02_ERASE_STALE_EPOCH';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Exercise the real SECURITY DEFINER executor with its normal FORCE RLS first.
 -- The nested rollback restores both rows for the independent predicate check.
 BEGIN
  n:=app.auth_erase_merchant_proofs(t,req,e);
  IF n<>expected_count OR EXISTS(SELECT FROM app.identity_merchant_proofs WHERE tenant_id=t) THEN
   RAISE EXCEPTION 'MDEL02_DERIVED_PROOF_SCRUB'; END IF;
  SELECT convert_to(jsonb_agg(to_jsonb(p) ORDER BY id)::text,'UTF8') INTO other_after
   FROM app.identity_merchant_proofs p WHERE tenant_id=b;
  IF other_after IS DISTINCT FROM other_before THEN RAISE EXCEPTION 'MDEL02_ERASE_OTHER_PROOF_CHANGED'; END IF;
  RAISE EXCEPTION 'restore normal-RLS witness' USING ERRCODE='P9001';
 EXCEPTION WHEN SQLSTATE 'P9001' THEN NULL; END;
 -- RLS and the explicit predicate are independent defences. This test-only
 -- permissive policy exposes both tenants to the SAME unassumable executor;
 -- it must still delete only p_tenant. No production role/policy is weakened.
 CREATE POLICY mdel02_predicate_witness ON app.identity_merchant_proofs
  TO app_identity_merchant_executor USING(true) WITH CHECK(true);
 n:=app.auth_erase_merchant_proofs(t,req,e);
 SELECT convert_to(jsonb_agg(to_jsonb(p) ORDER BY id)::text,'UTF8') INTO other_after
  FROM app.identity_merchant_proofs p WHERE tenant_id=b;
 IF n<>expected_count OR other_after IS DISTINCT FROM other_before THEN RAISE EXCEPTION 'MDEL02_ERASE_TENANT_PREDICATE'; END IF;
 IF EXISTS(SELECT FROM app.identity_merchant_proofs WHERE tenant_id=t) THEN RAISE EXCEPTION 'MDEL02_DERIVED_PROOF_SCRUB'; END IF;
 DROP POLICY mdel02_predicate_witness ON app.identity_merchant_proofs;
 IF app.auth_erase_merchant_proofs(t,req,e)<>0 THEN RAISE EXCEPTION 'MDEL02_SCRUB_NOT_IDEMPOTENT'; END IF;
 IF NOT app.auth_merchant_receipt(t,req,u,s) THEN RAISE EXCEPTION 'MDEL02_EXECUTING_OWNER_STATUS'; END IF;
 BEGIN
  PERFORM set_config('app.session_idle_seconds','60',true);
  WITH instant AS MATERIALIZED(SELECT clock_timestamp() at)
   UPDATE app.auth_sessions SET created_at=instant.at-interval '2 hours',last_seen_at=instant.at-interval '61 seconds' FROM instant WHERE id=s;
  PERFORM set_config('app.session_idle_seconds','120',true);
  IF app.auth_merchant_receipt(t,req,u,s) IS DISTINCT FROM true THEN
   RAISE EXCEPTION 'MDEL02_RECEIPT_CONFIGURED_IDLE'; END IF;
  PERFORM set_config('app.session_idle_seconds','60',true);
  IF app.auth_merchant_receipt(t,req,u,s) IS DISTINCT FROM false THEN
   RAISE EXCEPTION 'MDEL02_RECEIPT_IDLE_REVALIDATION'; END IF;
  RAISE EXCEPTION 'restore idle witness' USING ERRCODE='P9002';
 EXCEPTION WHEN SQLSTATE 'P9002' THEN NULL; END;
 PERFORM set_config('app.session_idle_seconds','',true);
 IF app.auth_merchant_receipt(t,req,u,s) IS DISTINCT FROM false THEN
  RAISE EXCEPTION 'MDEL02_RECEIPT_MISSING_IDLE_CONFIG'; END IF;
 PERFORM set_config('app.session_idle_seconds','1800',true);

 IF app.auth_merchant_receipt(t,req,current_setting('test.mdel02.admin')::uuid,current_setting('test.mdel02.admin_sess')::uuid)
  OR app.auth_merchant_receipt(t,uuidv7(),u,s) THEN RAISE EXCEPTION 'MDEL02_EXECUTING_STATUS_BINDING'; END IF;
 DELETE FROM app.tenant_memberships WHERE tenant_id=t AND user_id=u;
 IF app.auth_merchant_receipt(t,req,u,s) THEN RAISE EXCEPTION 'MDEL02_DETACHED_NONTERMINAL_STATUS'; END IF;
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'owner');
 UPDATE app.tenants SET status='suspended' WHERE id=t;
 IF app.auth_merchant_receipt(t,req,u,s) THEN RAISE EXCEPTION 'MDEL02_SUSPENDED_OWNER_STATUS'; END IF;
 UPDATE app.tenants SET status='active' WHERE id=t;
 PERFORM app.tenant_lifecycle_checkpoint(t,req,e,true);
 -- Detachment does not revoke a global session or another tenant's authority.
 DELETE FROM app.tenant_memberships WHERE tenant_id=t;
 IF NOT app.auth_merchant_receipt(t,req,u,s) OR app.auth_merchant_receipt(t,uuidv7(),u,s)
  OR app.auth_merchant_receipt(t,req,current_setting('test.mdel02.admin')::uuid,current_setting('test.mdel02.admin_sess')::uuid) THEN
  RAISE EXCEPTION 'MDEL02_FROZEN_REQUESTER_RECEIPT'; END IF;
 UPDATE app.auth_sessions SET revoked_at=clock_timestamp() WHERE id=s;
 IF app.auth_merchant_receipt(t,req,u,s) THEN RAISE EXCEPTION 'MDEL02_REVOKED_RECEIPT_SESSION'; END IF;
 UPDATE app.auth_sessions SET revoked_at=NULL WHERE id=s;
 PERFORM set_config('app.tenant_id',b::text,true);
 IF NOT app.auth_revalidate_tenant(b,u,'session',s,'catalog:write')
  OR NOT EXISTS(SELECT FROM app.auth_sessions WHERE id=s AND revoked_at IS NULL)
  OR (SELECT password_hash FROM app.users WHERE id=u)<>'replacement-phc' THEN
  RAISE EXCEPTION 'MDEL02_OTHER_TENANT_GLOBAL_SESSION'; END IF;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN
  PERFORM app.auth_issue_merchant_proof(t,u,s,'request',d,'replacement-phc');
  RAISE EXCEPTION 'MDEL02_MISSING_GUC';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $test$;
DO $acl$
BEGIN
 IF has_table_privilege('app_auth','app.identity_merchant_proofs','SELECT,INSERT,UPDATE,DELETE')
  OR has_table_privilege('app_runtime','app.identity_merchant_proofs','SELECT,INSERT,UPDATE,DELETE')
  OR has_function_privilege('app_auth','app.auth_consume_merchant_proof(uuid,uuid,uuid,text,bytea)','EXECUTE')
  OR has_function_privilege('app_runtime','app.auth_issue_merchant_proof(uuid,uuid,uuid,text,bytea,text)','EXECUTE')
  OR has_function_privilege('app_auth','app.auth_erase_merchant_proofs(uuid,uuid,bigint)','EXECUTE') THEN
  RAISE EXCEPTION 'MDEL02_RAW_PROOF_OR_WORKER_ACCESS'; END IF;
END $acl$;
-- A canceled request is active again. Its current owner may read metadata;
-- the detached frozen requester retains only the terminal receipt exception.
DO $canceled_receipt$
DECLARE t uuid:=current_setting('test.mdel02.b')::uuid; u uuid:=current_setting('test.mdel02.owner')::uuid;
 s uuid:=current_setting('test.mdel02.sess')::uuid; req uuid:=uuidv7(); metadata jsonb;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 metadata:=app.tenant_lifecycle_transition(t,req,u,s,'request',uuidv7(),sha256('receipt-request'::bytea),0,
  '{"paid_unfulfilled":0,"fulfillment_return":0,"payment_refund":0,"checkout_capture":0,"refund_reconciliation":0,"pending_payout":0,"unsettled_provider_balance":0,"legal_hold":0}');
 PERFORM app.tenant_lifecycle_transition(t,req,u,s,'cancel',uuidv7(),sha256('receipt-cancel'::bytea),(metadata->>'epoch')::bigint,'{}');
 DELETE FROM app.tenant_memberships WHERE tenant_id=t AND user_id=u;
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,current_setting('test.mdel02.admin')::uuid,'owner');
 IF NOT app.auth_merchant_receipt(t,req,current_setting('test.mdel02.admin')::uuid,current_setting('test.mdel02.admin_sess')::uuid)
  OR NOT app.auth_merchant_receipt(t,req,u,s) OR app.auth_merchant_receipt(t,uuidv7(),u,s) THEN
  RAISE EXCEPTION 'MDEL02_CANCELED_CURRENT_OWNER_RECEIPT'; END IF;
END $canceled_receipt$;
-- Empty tenant removal cascades proof custody, preserving global credentials.
INSERT INTO app.tenants(name) VALUES('mdel02-empty-remove') RETURNING id AS empty_tenant \gset
SELECT set_config('app.tenant_id',:'empty_tenant',true);
INSERT INTO app.identity_merchant_proofs(tenant_id,user_id,session_id,action,proof_digest,created_at,expires_at)
 SELECT :'empty_tenant',:'owner_id',:'sess','request',sha256('cascade'::bytea),at_db,at_db+interval '5 minutes'
 FROM (SELECT clock_timestamp() at_db) clock;
DO $remove_tenant$
BEGIN
 DELETE FROM app.tenants WHERE id=current_setting('app.tenant_id')::uuid;
EXCEPTION WHEN foreign_key_violation THEN
 RAISE EXCEPTION 'MDEL02_PROOF_TENANT_CASCADE';
END $remove_tenant$;
DO $cascade$
BEGIN
 IF EXISTS(SELECT FROM app.identity_merchant_proofs WHERE tenant_id=current_setting('app.tenant_id')::uuid)
  OR NOT EXISTS(SELECT FROM app.auth_sessions WHERE id=current_setting('test.mdel02.sess')::uuid) THEN
  RAISE EXCEPTION 'MDEL02_PROOF_TENANT_CASCADE'; END IF;
END $cascade$;
ROLLBACK;
SELECT 'PASS: MDEL02 proof binding/expiry/revocation, session fence, scrub and frozen requester receipt';
