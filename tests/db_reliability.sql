-- Responsibility: real-PG reliability storage, fencing, replay and audit gates.
-- Dependencies: all forward migrations; privileged disposable-DB psql runner.
-- Caller: check_pg.sh. Invariants: tenant scope, deduplication, immutable request
-- identity, terminal results and expired/previous-epoch workers cannot acknowledge.
\set ON_ERROR_STOP on
\if :{?reliability_case}
\else
 \set reliability_case all
\endif
SELECT set_config('test.reliability_case', :'reliability_case', false);
BEGIN;
CREATE FUNCTION pg_temp.assert_true(ok boolean, label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 IF ok IS DISTINCT FROM true THEN RAISE EXCEPTION 'RELIABILITY_GATE %',label; END IF;
END $$;
CREATE FUNCTION pg_temp.reject_sql(q text, expected text) RETURNS void
LANGUAGE plpgsql AS $$ DECLARE got text; BEGIN
 BEGIN EXECUTE q; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS got=RETURNED_SQLSTATE; END;
 IF got IS DISTINCT FROM expected THEN
  RAISE EXCEPTION 'RELIABILITY_GATE expected SQLSTATE %, got % for %',expected,coalesce(got,'success'),q;
 END IF;
END $$;
DO $$ DECLARE c text:=current_setting('test.reliability_case'); BEGIN
 IF c IN ('all','inbox') AND to_regclass('app.webhook_inbox') IS NULL THEN
  RAISE EXCEPTION 'RELIABILITY_MISSING inbox';
 END IF;
 IF c IN ('all','outbox') AND NOT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.outbox_events'::regclass AND attname='lease_owner' AND NOT attisdropped) THEN
  RAISE EXCEPTION 'RELIABILITY_MISSING outbox lease';
 END IF;
 IF c IN ('all','idempotency') AND NOT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.idempotency_records'::regclass AND attname='claim_token' AND NOT attisdropped) THEN
  RAISE EXCEPTION 'RELIABILITY_MISSING idempotency claim';
 END IF;
END $$;
INSERT INTO app.tenants(name) VALUES('reliability-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('reliability-B') RETURNING id AS tb \gset
SELECT uuidv7() AS worker, uuidv7() AS other_worker, uuidv7() AS actor, uuidv7() AS token \gset
SELECT uuidv7() AS token2, uuidv7() AS token3, uuidv7() AS token4 \gset
SELECT set_config('app.tenant_id', :'ta', true);
SELECT current_setting('test.reliability_case') IN ('all','inbox') AS do_inbox,
 current_setting('test.reliability_case') IN ('all','outbox') AS do_outbox,
 current_setting('test.reliability_case') IN ('all','idempotency') AS do_idempotency \gset
\if :do_inbox
SET LOCAL ROLE app_runtime;
INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(:'ta','test','acct','test','e1',sha256('event'::bytea),'blob:test/e1') RETURNING id AS inbox \gset
SELECT pg_temp.reject_sql(format('INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference) VALUES(%L,%L,%L,%L,%L,sha256(%L::bytea),%L)',:'ta','test','acct','test','e1','event','blob:other'), '23505');
-- Provider/account/environment and tenant are independent namespaces.
INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(:'ta','test','other','test','e1',sha256('event'::bytea),'blob:other'),
       (:'ta','test','acct','live','e1',sha256('event'::bytea),'blob:live'),
       (:'ta','other','acct','test','e1',sha256('event'::bytea),'blob:provider');
SELECT pg_temp.reject_sql(format('INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference) VALUES(%L,%L,%L,%L,%L,sha256(%L::bytea),%L)',:'tb','test','acct','test','e1','event','blob:b'), '42501');
SELECT pg_temp.reject_sql(format('UPDATE app.webhook_inbox SET status=%L WHERE id=%L','invalid',:'inbox'),'23514');
SELECT pg_temp.reject_sql(format('UPDATE app.webhook_inbox SET status=%L,processed_at=now() WHERE id=%L','processed',:'inbox'),'55000');
UPDATE app.webhook_inbox SET status='processing',attempts=attempts+1 WHERE id=:'inbox';
UPDATE app.webhook_inbox SET status='processed',processed_at=clock_timestamp() WHERE id=:'inbox';
SELECT pg_temp.reject_sql(format('UPDATE app.webhook_inbox SET status=%L,processed_at=NULL WHERE id=%L','pending',:'inbox'),'55000');
SELECT pg_temp.reject_sql(format('UPDATE app.webhook_inbox SET payload_reference=%L WHERE id=%L','changed',:'inbox'),'42501');
RESET ROLE;
INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(:'tb','test','acct','test','e1',sha256('event'::bytea),'blob:b');
SET LOCAL ROLE app_runtime;
SELECT pg_temp.assert_true((SELECT count(*)=4 FROM app.webhook_inbox),'inbox hides tenant B');
RESET ROLE;
SELECT pg_temp.assert_true((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='app.webhook_inbox'::regclass),'inbox FORCE RLS');
SELECT pg_temp.assert_true(NOT EXISTS(SELECT FROM pg_attribute WHERE attrelid='app.webhook_inbox'::regclass AND attnum>0 AND NOT attisdropped AND (attname='payload' OR atttypid='jsonb'::regtype)),'inbox hash/reference only');
SELECT 'PASS: reliability inbox identity/dedup/status/terminal/RLS' AS result;
\endif
\if :do_outbox
SET LOCAL ROLE app_runtime;
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(:'ta','test','{}','test:destination','one') RETURNING id AS event \gset
SELECT pg_temp.reject_sql(format('INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(%L,%L,%L,%L,%L)',:'ta','test','{}','test:destination','one'),'23505');
SELECT pg_temp.reject_sql(format('UPDATE app.outbox_events SET status=%L WHERE id=%L','delivered',:'event'),'42501');
SELECT pg_temp.reject_sql(format('INSERT INTO app.outbox_events(tenant_id,event_type,payload,status) VALUES(%L,%L,%L,%L)',:'ta','test','{}','delivered'),'42501');
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.reject_sql(format('SELECT app.claim_outbox(%L,%L,1,60)',:'tb',:'worker'),'42501');
SELECT pg_temp.reject_sql(format('SELECT app.claim_outbox(%L,%L,0,60)',:'ta',:'worker'),'22023');
SELECT pg_temp.assert_true((SELECT count(*)=1 FROM app.claim_outbox(:'ta',:'worker',1,60)),'due event claimed');
SELECT pg_temp.assert_true(NOT app.complete_outbox(:'ta',:'event',:'other_worker',1),'different worker fenced');
SELECT pg_temp.assert_true(NOT app.complete_outbox(:'ta',:'event',:'worker',0),'wrong epoch fenced');
RESET ROLE;
UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE id=:'event';
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_true(NOT app.complete_outbox(:'ta',:'event',:'worker',1),'expired worker fenced before reclaim');
SELECT pg_temp.assert_true(NOT app.fail_outbox(:'ta',:'event',:'worker',1,'late',0),'expired fail fenced');
SELECT pg_temp.assert_true((SELECT attempts=2 AND lease_owner=:'worker'::uuid FROM app.claim_outbox(:'ta',:'worker',1,60)),'expired lease reclaimed with new epoch');
SELECT pg_temp.assert_true(NOT app.complete_outbox(:'ta',:'event',:'worker',1),'same worker previous epoch fenced');
SELECT pg_temp.assert_true(app.fail_outbox(:'ta',:'event',:'worker',2,'controlled test failure',3600),'current worker failure accepted');
SELECT pg_temp.assert_true((SELECT count(*)=0 FROM app.claim_outbox(:'ta',:'worker',1,60)),'retry not due');
RESET ROLE;
UPDATE app.outbox_events SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=:'event';
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_true((SELECT attempts=3 FROM app.claim_outbox(:'ta',:'worker',1,60)),'failed event retried');
SELECT pg_temp.assert_true(app.complete_outbox(:'ta',:'event',:'worker',3),'current completion accepted');
SELECT pg_temp.assert_true(NOT app.fail_outbox(:'ta',:'event',:'worker',3,'late',0),'delivered cannot fail');
SELECT pg_temp.assert_true((SELECT count(*)=0 FROM app.claim_outbox(:'ta',:'worker',10,60)),'delivered cannot reclaim');
RESET ROLE;
SELECT pg_temp.assert_true((SELECT status='delivered' AND attempts=3 AND delivered_at IS NOT NULL AND lease_owner IS NULL AND lease_until IS NULL AND last_error IS NULL FROM app.outbox_events WHERE id=:'event'),'exact delivery witness');
SELECT 'PASS: reliability outbox producer boundaries/lease expiry/epoch/retry/completion' AS result;
\endif
\if :do_idempotency
SET LOCAL ROLE app_runtime;
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'ta','service',:'actor','POST /test/{id}','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('POST|/test/{id}|id=1|q=1|{}'::bytea),now()+interval '1 day',:'token');
-- Supplying a fresh token identifies the winner without xmax/visibility guesses.
WITH r AS (INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'ta','service',:'actor','POST /test/{id}','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('POST|/test/{id}|id=1|q=1|{}'::bytea),now()+interval '1 day',uuidv7())
 ON CONFLICT(tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING *)
SELECT pg_temp.assert_true((SELECT claim_token=:'token'::uuid AND state='in_progress' FROM r),'same request returns existing in_progress ->409');
WITH r AS (INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at)
 VALUES(:'ta','service',:'actor','POST /test/{id}','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('POST|/test/{id}|id=2|q=1|{}'::bytea),now()+interval '1 day')
 ON CONFLICT(tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING *)
SELECT pg_temp.assert_true((SELECT request_hash<>sha256('POST|/test/{id}|id=2|q=1|{}'::bytea) FROM r),'changed request returns original hash ->422');
SELECT pg_temp.reject_sql(format('UPDATE app.idempotency_records SET request_hash=sha256(%L::bytea)','changed'),'55000');
-- Valid claim token, so the CHECK on response shape (not the fence) is what rejects.
SELECT set_config('app.idempotency_claim',:'token',true);
SELECT pg_temp.reject_sql('UPDATE app.idempotency_records SET state=''completed''','23514');
-- Completion is fenced: only the holder of the current claim token may complete.
SELECT set_config('app.idempotency_claim',uuidv7()::text,true);
SELECT pg_temp.reject_sql('UPDATE app.idempotency_records SET state=''completed'',response_status=201,response_body_reference=''blob:x'',completed_at=clock_timestamp() WHERE idempotency_key=''ac585171-8d9c-5d11-8feb-a32bc62aa7ad''','55000');
SELECT set_config('app.idempotency_claim',:'token',true);
UPDATE app.idempotency_records SET state='completed',response_status=201,response_body_reference='blob:response/one',completed_at=clock_timestamp() WHERE idempotency_key='ac585171-8d9c-5d11-8feb-a32bc62aa7ad';
WITH r AS (INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at)
 VALUES(:'ta','service',:'actor','POST /test/{id}','ac585171-8d9c-5d11-8feb-a32bc62aa7ad',sha256('POST|/test/{id}|id=1|q=1|{}'::bytea),now()+interval '1 day')
 ON CONFLICT(tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING *)
SELECT pg_temp.assert_true((SELECT state='completed' AND response_status=201 AND response_body_reference='blob:response/one' FROM r),'completed exact replay');
SELECT pg_temp.reject_sql('UPDATE app.idempotency_records SET response_status=202','55000');
SELECT pg_temp.reject_sql('UPDATE app.idempotency_records SET state=''in_progress'',response_status=NULL,response_body_reference=NULL,completed_at=NULL','55000');
-- Crashed owner: an expired in_progress claim for the SAME request is reacquired
-- atomically with a fresh token; the stale owner can no longer complete.
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'ta','service',:'actor','POST /test/{id}','2768b5da-9242-5e38-a4c4-7bda32f9f96f',sha256('two'::bytea),now()-interval '1 second',:'token2');
SELECT pg_temp.assert_true((SELECT claim_token=:'token3'::uuid AND state='in_progress' AND expires_at>now()
 FROM app.idempotency_claim(:'ta','service',:'actor','POST /test/{id}','2768b5da-9242-5e38-a4c4-7bda32f9f96f',sha256('two'::bytea),interval '1 hour',:'token3')),'expired same-request claim is reacquired');
SELECT set_config('app.idempotency_claim',:'token2',true);
SELECT pg_temp.reject_sql('UPDATE app.idempotency_records SET state=''completed'',response_status=201,response_body_reference=''blob:stale'',completed_at=clock_timestamp() WHERE idempotency_key=''2768b5da-9242-5e38-a4c4-7bda32f9f96f''','55000');
SELECT set_config('app.idempotency_claim',:'token3',true);
UPDATE app.idempotency_records SET state='completed',response_status=200,response_body_reference='blob:response/two',completed_at=clock_timestamp() WHERE idempotency_key='2768b5da-9242-5e38-a4c4-7bda32f9f96f';
-- Expired claim with a DIFFERENT request is not taken over (caller answers 422).
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'ta','service',:'actor','POST /test/{id}','670f49da-d730-5001-8754-7f4b62db1452',sha256('three'::bytea),now()-interval '1 second',:'token4');
SELECT pg_temp.assert_true((SELECT claim_token=:'token4'::uuid AND request_hash=sha256('three'::bytea)
 FROM app.idempotency_claim(:'ta','service',:'actor','POST /test/{id}','670f49da-d730-5001-8754-7f4b62db1452',sha256('other'::bytea),interval '1 hour',uuidv7())),'expired different-request claim is not reacquired');
-- A live claim is never stolen; reclaim cannot be faked by a direct UPDATE.
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'ta','service',:'actor','POST /test/{id}','46b3f5bb-00f0-5014-848a-3cdae4a9d69a',sha256('four'::bytea),now()+interval '1 hour',uuidv7());
SELECT pg_temp.assert_true((SELECT claim_token<>:'token3'::uuid FROM app.idempotency_claim(:'ta','service',:'actor','POST /test/{id}','46b3f5bb-00f0-5014-848a-3cdae4a9d69a',sha256('four'::bytea),interval '1 hour',:'token3')),'live claim is not stolen');
SELECT pg_temp.reject_sql(format('UPDATE app.idempotency_records SET claim_token=%L WHERE idempotency_key=''46b3f5bb-00f0-5014-848a-3cdae4a9d69a''',uuidv7()),'55000');
RESET ROLE;
SELECT 'PASS: reliability idempotency atomic existing-row/409/422/replay/immutability' AS result;
\endif
SET LOCAL ROLE app_runtime;
INSERT INTO app.audit_events(tenant_id,actor_kind,operation,resource_type) VALUES(:'ta','service','reliability-audit','test');
SELECT pg_temp.reject_sql('UPDATE app.audit_events SET operation=''tampered''','42501');
SELECT pg_temp.reject_sql('DELETE FROM app.audit_events','42501');
RESET ROLE;
SELECT pg_temp.assert_true(NOT EXISTS(SELECT FROM pg_auth_members WHERE member='app_runtime'::regrole),'runtime has no inherited audit capability');
SELECT 'PASS: reliability audit append-only (including inheritance)' AS result;
ROLLBACK;
