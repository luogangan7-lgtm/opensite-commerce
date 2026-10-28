-- Responsibility: Identity merchant-deletion proof, fencing and receipt witnesses.
-- Dependencies: all forward migrations; real disposable PostgreSQL 18 CI only.
-- Invariants: tenant/session/action binding, one use, DB expiry after locks,
-- other-tenant global accounts survive, no application role can consume proofs.
\set ON_ERROR_STOP on
BEGIN;
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
 req uuid:=uuidv7(); e bigint; n bigint;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
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
 -- Lifecycle state is controlled by Tenant. Set a worker-admitted fixture;
 -- Identity never writes this table in production.
 PERFORM app.tenant_lifecycle_transition(t,req,u,s,'request',uuidv7(),sha256('request'::bytea),0,
  '{"paid_unfulfilled":0,"fulfillment_return":0,"payment_refund":0,"checkout_capture":0,"refund_reconciliation":0,"pending_payout":0,"unsettled_provider_balance":0,"legal_hold":0}');
 IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:write') THEN RAISE EXCEPTION 'MDEL02_COOLING_WRITE'; END IF;
 IF NOT app.auth_revalidate_tenant(t,u,'session',s,'catalog:read') THEN RAISE EXCEPTION 'MDEL02_COOLING_READ'; END IF;
 IF NOT app.auth_revalidate_tenant(t,u,'session',s,'tenant:delete') THEN RAISE EXCEPTION 'MDEL02_CANCEL_AUTHORITY'; END IF;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing',epoch=epoch+1 WHERE tenant_id=t RETURNING epoch INTO e;
 IF app.auth_revalidate_tenant(t,u,'session',s,'catalog:read') OR app.auth_revalidate_tenant(t,u,'session',s,'tenant:delete') THEN
  RAISE EXCEPTION 'MDEL02_EXECUTION_SESSION_FENCE'; END IF;
 BEGIN
  PERFORM app.auth_erase_merchant_proofs(b,req,e);
  RAISE EXCEPTION 'MDEL02_ERASE_TENANT_PREDICATE';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.auth_erase_merchant_proofs(t,req,e-1);
  RAISE EXCEPTION 'MDEL02_ERASE_STALE_EPOCH';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 n:=app.auth_erase_merchant_proofs(t,req,e);
 IF n=0 OR EXISTS(SELECT FROM app.identity_merchant_proofs WHERE tenant_id=t) THEN RAISE EXCEPTION 'MDEL02_DERIVED_PROOF_SCRUB'; END IF;
 IF app.auth_erase_merchant_proofs(t,req,e)<>0 THEN RAISE EXCEPTION 'MDEL02_SCRUB_NOT_IDEMPOTENT'; END IF;
 IF app.auth_merchant_receipt(t,req,u,s) THEN RAISE EXCEPTION 'MDEL02_NONTERMINAL_RECEIPT'; END IF;
 PERFORM app.tenant_lifecycle_checkpoint(t,req,e,true);
 -- Detachment does not revoke a global session or another tenant's authority.
 DELETE FROM app.tenant_memberships WHERE tenant_id=t;
 IF NOT app.auth_merchant_receipt(t,req,u,s) OR app.auth_merchant_receipt(t,uuidv7(),u,s)
  OR app.auth_merchant_receipt(t,req,current_setting('test.mdel02.admin')::uuid,current_setting('test.mdel02.admin_sess')::uuid) THEN
  RAISE EXCEPTION 'MDEL02_FROZEN_REQUESTER_RECEIPT'; END IF;
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
ROLLBACK;
SELECT 'PASS: MDEL02 proof binding/expiry/revocation, session fence, scrub and frozen requester receipt';
