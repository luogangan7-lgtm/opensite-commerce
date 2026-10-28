-- Responsibility: pin Tenant-owned lifecycle, replay, fence and admission semantics.
-- Dependencies: all forward migrations; privileged disposable PG18 CI runner.
-- Invariants: DB time after fence; one active lifecycle; owner-only transitions;
-- no ordinary write or stale drain after admission; missing GUC denies safely.
\set ON_ERROR_STOP on
BEGIN;
DO $schema$
BEGIN
 IF to_regclass('app.tenant_lifecycles') IS NULL THEN
  RAISE EXCEPTION 'MDEL01_SCHEMA_REQUIRED';
 END IF;
END $schema$;
INSERT INTO app.tenants(name) VALUES('mdel01-a') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('mdel01-b') RETURNING id AS tb \gset
INSERT INTO app.users(email,password_hash) VALUES('mdel01-owner@invalid.test','fixture') RETURNING id AS owner_id \gset
INSERT INTO app.users(email,password_hash) VALUES('mdel01-admin@invalid.test','fixture') RETURNING id AS admin_id \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'ta',:'owner_id','owner'),(:'ta',:'admin_id','admin'),(:'tb',:'owner_id','owner');
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'owner_id',sha256('mdel01-owner'::bytea),clock_timestamp()+interval '1 day') RETURNING id AS owner_session \gset
INSERT INTO app.auth_sessions(user_id,token_hash,expires_at) VALUES(:'admin_id',sha256('mdel01-admin'::bytea),clock_timestamp()+interval '1 day') RETURNING id AS admin_session \gset
SELECT set_config('test.mdel01.a',:'ta',true),set_config('test.mdel01.b',:'tb',true),
 set_config('test.mdel01.owner',:'owner_id',true),set_config('test.mdel01.admin',:'admin_id',true),
 set_config('test.mdel01.session',:'owner_session',true),set_config('test.mdel01.admin_session',:'admin_session',true);
DO $test$
DECLARE t uuid:=current_setting('test.mdel01.a')::uuid; other uuid:=current_setting('test.mdel01.b')::uuid;
 actor uuid:=current_setting('test.mdel01.owner')::uuid; sess uuid:=current_setting('test.mdel01.session')::uuid;
 req uuid:=uuidv7(); key uuid:=uuidv7(); cancel_key uuid:=uuidv7(); before_db timestamptz;
 r jsonb; replay jsonb; row_value app.tenant_lifecycles; epoch bigint;
 zeroes jsonb:='{"paid_unfulfilled":0,"fulfillment_return":0,"payment_refund":0,"checkout_capture":0,"refund_reconciliation":0,"pending_payout":0,"unsettled_provider_balance":0,"legal_hold":0}';
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 -- Unknown payout/hold authority and every nonzero category deny admission.
 r:=app.tenant_lifecycle_transition(t,req,actor,sess,'request',key,sha256('request'::bytea),0,'{}');
 IF r->>'code'<>'tenant_deletion_blocked' OR (SELECT state FROM app.tenant_lifecycles WHERE tenant_id=t)<>'active' THEN
  RAISE EXCEPTION 'MDEL01_UNKNOWN_AUTHORITY_ACCEPTED'; END IF;
 FOR r IN SELECT jsonb_build_object(k,1) FROM jsonb_object_keys(zeroes) k LOOP
  replay:=app.tenant_lifecycle_transition(t,req,actor,sess,'request',key,sha256('request'::bytea),0,zeroes||r);
  IF replay->>'code'<>'tenant_deletion_blocked' THEN RAISE EXCEPTION 'MDEL01_OBLIGATION_ACCEPTED %',r; END IF;
 END LOOP;
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,req,current_setting('test.mdel01.admin')::uuid,current_setting('test.mdel01.admin_session')::uuid,'request',key,sha256('request'::bytea),0,zeroes);
  RAISE EXCEPTION 'MDEL01_ADMIN_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 before_db:=clock_timestamp();
 r:=app.tenant_lifecycle_transition(t,req,actor,sess,'request',key,sha256('request'::bytea),0,zeroes);
 SELECT * INTO STRICT row_value FROM app.tenant_lifecycles WHERE tenant_id=t;
 IF row_value.state<>'cooling_off' OR row_value.requested_at<before_db
  OR row_value.execute_after-row_value.requested_at<>interval '720 hours' THEN RAISE EXCEPTION 'MDEL01_REQUEST_DB_TIME'; END IF;
 epoch:=row_value.epoch;
 replay:=app.tenant_lifecycle_transition(t,req,actor,sess,'request',key,sha256('request'::bytea),0,zeroes);
 IF replay IS DISTINCT FROM r THEN RAISE EXCEPTION 'MDEL01_REPLAY_CHANGED'; END IF;
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,req,actor,sess,'request',key,sha256('different'::bytea),0,zeroes);
  RAISE EXCEPTION 'MDEL01_CHANGED_BODY_ACCEPTED';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,uuidv7(),actor,sess,'request',uuidv7(),sha256('request'::bytea),epoch,zeroes);
  RAISE EXCEPTION 'MDEL01_SECOND_REQUEST_ACCEPTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  PERFORM app.tenant_lifecycle_write_fence(t);
  RAISE EXCEPTION 'MDEL01_WRITE_FENCE_OMITTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 PERFORM app.tenant_lifecycle_drain_fence(t,epoch);
 BEGIN
  PERFORM app.tenant_lifecycle_drain_fence(t,epoch-1);
  RAISE EXCEPTION 'MDEL01_STALE_DRAIN_ACCEPTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,req,actor,sess,'execute',NULL,NULL,epoch,zeroes);
  RAISE EXCEPTION 'MDEL01_EARLY_EXECUTE_ACCEPTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Cancel is a lifecycle primitive. Serving restoration/export cleanup are
 -- composed in the same transaction by DataRights/Publish before activation.
 replay:=app.tenant_lifecycle_transition(t,req,actor,sess,'cancel',cancel_key,sha256('cancel'::bytea),epoch,zeroes);
 IF replay->>'state'<>'active' THEN RAISE EXCEPTION 'MDEL01_CANCEL_NOT_ACTIVE'; END IF;
 IF app.tenant_lifecycle_transition(t,req,actor,sess,'cancel',cancel_key,sha256('cancel'::bytea),epoch,zeroes) IS DISTINCT FROM replay THEN
  RAISE EXCEPTION 'MDEL01_CANCEL_REPLAY'; END IF;
 IF (SELECT name FROM app.tenants WHERE id=other)<>'mdel01-b'
  OR (SELECT state FROM app.tenant_lifecycles WHERE tenant_id=other)<>'active' THEN RAISE EXCEPTION 'MDEL01_CROSS_TENANT_CHANGED'; END IF;
 -- Deadline equality is tested with a persisted DB deadline, no wall-clock sleeps.
 req:=uuidv7();
 r:=app.tenant_lifecycle_transition(t,req,actor,sess,'request',uuidv7(),sha256('again'::bytea),0,zeroes);
 UPDATE app.tenant_lifecycles SET requested_at=clock_timestamp()-interval '720 hours',execute_after=clock_timestamp() WHERE tenant_id=t;
 SELECT epoch INTO epoch FROM app.tenant_lifecycles l WHERE l.tenant_id=t;
 BEGIN
  PERFORM app.tenant_lifecycle_transition(t,req,actor,sess,'cancel',uuidv7(),sha256('late'::bytea),epoch,zeroes);
  RAISE EXCEPTION 'MDEL01_DEADLINE_CANCEL_ACCEPTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 r:=app.tenant_lifecycle_transition(t,req,actor,sess,'execute',NULL,NULL,epoch,zeroes||'{"pending_payout":1}');
 IF r->>'state'<>'failed' OR r->>'phase'<>'preconditions' THEN RAISE EXCEPTION 'MDEL01_EXECUTION_BLOCKER_ERASE_STARTED'; END IF;
 SELECT * INTO STRICT row_value FROM app.tenant_lifecycles WHERE tenant_id=t;
 IF row_value.next_retry_at IS NULL OR row_value.execute_after>clock_timestamp() THEN RAISE EXCEPTION 'MDEL01_FAILURE_RESTARTED_COOLING'; END IF;
 r:=app.tenant_lifecycle_transition(t,req,actor,sess,'execute',NULL,NULL,row_value.epoch,zeroes);
 IF r->>'state'<>'executing' THEN RAISE EXCEPTION 'MDEL01_RETRY_NOT_EXECUTING'; END IF;
 BEGIN
  PERFORM app.tenant_lifecycle_drain_fence(t,(r->>'epoch')::bigint);
  RAISE EXCEPTION 'MDEL01_EXECUTING_DRAIN_ACCEPTED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $test$;
-- Runtime has read-only status, no transitions or drain capability. Empty and
-- missing GUC returns zero rows; explicitly naming another tenant never helps.
SET LOCAL ROLE app_runtime;
SELECT set_config('app.tenant_id','',true);
DO $rls$ BEGIN
 IF EXISTS(SELECT FROM app.tenant_lifecycles) THEN RAISE EXCEPTION 'MDEL01_EMPTY_GUC_LEAK'; END IF;
 BEGIN
  PERFORM app.tenant_lifecycle_write_fence(current_setting('test.mdel01.a')::uuid);
  RAISE EXCEPTION 'MDEL01_MISSING_GUC_WRITE';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $rls$;
SELECT set_config('app.tenant_id',current_setting('test.mdel01.b'),true);
DO $rls$ BEGIN
 IF EXISTS(SELECT FROM app.tenant_lifecycles WHERE tenant_id=current_setting('test.mdel01.a')::uuid) THEN
  RAISE EXCEPTION 'MDEL01_CROSS_TENANT_READ'; END IF;
 IF has_function_privilege(current_user,'app.tenant_lifecycle_transition(uuid,uuid,uuid,uuid,text,uuid,bytea,bigint,jsonb)','EXECUTE')
  OR has_function_privilege(current_user,'app.tenant_lifecycle_drain_fence(uuid,bigint)','EXECUTE') THEN
  RAISE EXCEPTION 'MDEL01_RUNTIME_TRANSITION_AUTHORITY'; END IF;
END $rls$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: MDEL01 lifecycle, DB deadline, replay, obligations, RLS and drain fencing';
