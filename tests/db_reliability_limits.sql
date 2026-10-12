-- Responsibility: real-PG TTL, finite retries and IDs-only tenant discovery gate.
-- Dependencies: forward migrations; privileged disposable psql runner.
-- Caller: check_pg.sh. Invariants: invalid TTL never owns a key, attempt eleven
-- never sends, final crash becomes queryable dead_letter, broker leaks no payload.
\set ON_ERROR_STOP on
\if :{?limits_case}
\else
 \set limits_case all
\endif
BEGIN;
SELECT set_config('test.limits_case', :'limits_case', true);
CREATE FUNCTION pg_temp.assert_limits(ok boolean,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 IF ok IS DISTINCT FROM true THEN RAISE EXCEPTION 'RELIABILITY_LIMITS_GATE %',label; END IF;
END $$;
CREATE FUNCTION pg_temp.reject_limits(q text,expected text,label text) RETURNS void
LANGUAGE plpgsql AS $$ DECLARE got text; BEGIN
 BEGIN EXECUTE q; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS got=RETURNED_SQLSTATE; END;
 IF got IS DISTINCT FROM expected THEN
  RAISE EXCEPTION 'RELIABILITY_LIMITS_GATE % expected %, got %',label,expected,coalesce(got,'success');
 END IF;
END $$;
INSERT INTO app.tenants(name) VALUES('limits-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('limits-B') RETURNING id AS tb \gset
SELECT set_config('app.tenant_id', :'ta', true),set_config('test.limits_ta', :'ta', true),set_config('test.limits_tb', :'tb', true);
SELECT current_setting('test.limits_case') IN ('all','ttl') AS ttl,
 current_setting('test.limits_case') IN ('all','cap') AS cap,
 current_setting('test.limits_case') IN ('all','tenants') AS tenants \gset
\if :ttl
SET LOCAL ROLE app_runtime;
DO $$ DECLARE v text; BEGIN
 FOREACH v IN ARRAY ARRAY['1 year -359 days','-1 year 361 days','24 hours 1 microsecond','0 seconds','-1 second','infinity','-infinity'] LOOP
  PERFORM pg_temp.reject_limits(format('SELECT app.idempotency_claim(%L,%L,uuidv7(),%L,%L,sha256(%L::bytea),%L::interval,uuidv7())',
   current_setting('test.limits_ta'),'api_key','limits','key','hash',v),'22023','TTL_REJECT_'||v);
 END LOOP;
 PERFORM pg_temp.reject_limits(format('SELECT app.idempotency_claim(%L,%L,uuidv7(),%L,%L,sha256(%L::bytea),NULL,uuidv7())',
  current_setting('test.limits_ta'),'api_key','limits','key','hash'),'22023','TTL_REJECT_NULL');
 PERFORM pg_temp.assert_limits((SELECT count(*)=0 FROM app.idempotency_records),'TTL_REJECT_NO_INSERT');
 FOREACH v IN ARRAY ARRAY['24 hours','1 day','1 month -29 days','1 year -364.25 days'] LOOP
  PERFORM pg_temp.assert_limits((app.idempotency_claim(current_setting('test.limits_ta')::uuid,'api_key',uuidv7(),'limits','key',sha256('hash'::bytea),v::interval,uuidv7())).state='in_progress','TTL_MAX_ACCEPTED_'||v);
 END LOOP;
END $$;
RESET ROLE;
\endif
\if :cap
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(:'ta','limits.retry','{}','test','retry') RETURNING id AS retry_event \gset
SELECT set_config('test.limits_retry', :'retry_event', true);
SET LOCAL ROLE app_outbox_worker;
DO $$ DECLARE i integer; e app.outbox_events; worker uuid:=uuidv7(); tid uuid:=current_setting('test.limits_ta')::uuid; BEGIN
 FOR i IN 1..10 LOOP
  SELECT * INTO e FROM app.claim_outbox(tid,worker,1,60);
  PERFORM pg_temp.assert_limits(e.id=current_setting('test.limits_retry')::uuid AND e.attempts=i,'CAP_EXPECTED_ATTEMPT_'||i);
  PERFORM pg_temp.assert_limits(NOT app.fail_outbox(tid,e.id,uuidv7(),i,'OUTBOX_SEND_FAILED',0),'CAP_WRONG_OWNER_FENCE');
  PERFORM pg_temp.assert_limits(NOT app.complete_outbox(tid,e.id,worker,i-1),'CAP_WRONG_ATTEMPT_FENCE');
  PERFORM pg_temp.assert_limits(app.fail_outbox(tid,e.id,worker,i,'OUTBOX_SEND_FAILED',0),'CAP_ACCEPTED_FAIL_'||i);
 END LOOP;
 PERFORM pg_temp.assert_limits((SELECT count(*)=0 FROM app.claim_outbox(tid,worker,1,60)),'CAP_NO_ATTEMPT_ELEVEN');
END $$;
RESET ROLE;
SELECT pg_temp.assert_limits((SELECT status='dead_letter' AND attempts=10 AND lease_owner IS NULL AND lease_until IS NULL AND last_error='OUTBOX_SEND_FAILED' FROM app.outbox_events WHERE id=:'retry_event'),'CAP_DLQ_QUERYABLE');
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key,attempts)
 VALUES(:'ta','limits.crash','{}','test','crash',9) RETURNING id AS crash_event \gset
SELECT uuidv7() AS worker \gset
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_limits((SELECT attempts=10 FROM app.claim_outbox(:'ta',:'worker',1,60)),'CAP_FINAL_LEASE');
RESET ROLE;
UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE id=:'crash_event';
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_limits(NOT app.complete_outbox(:'ta',:'crash_event',:'worker',10),'CAP_EXPIRED_FINAL_ACK_FENCE');
SELECT pg_temp.assert_limits(NOT app.fail_outbox(:'ta',:'crash_event',:'worker',10,'OUTBOX_SEND_FAILED',0),'CAP_EXPIRED_FINAL_FAIL_FENCE');
SELECT pg_temp.assert_limits((SELECT count(*)=0 FROM app.claim_outbox(:'ta',:'worker',1,60)),'CAP_CRASH_NO_ATTEMPT_ELEVEN');
RESET ROLE;
SELECT pg_temp.assert_limits((SELECT status='dead_letter' AND attempts=10 AND lease_owner IS NULL AND lease_until IS NULL FROM app.outbox_events WHERE id=:'crash_event'),'CAP_CRASH_DLQ_QUERYABLE');
-- A successful send on the final unexpired lease may still acknowledge normally.
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key,attempts)
 VALUES(:'ta','limits.final','{}','test','final',9) RETURNING id AS final_event \gset
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_limits((SELECT attempts=10 FROM app.claim_outbox(:'ta',:'worker',1,60)),'CAP_FINAL_SUCCESS_LEASE');
SELECT pg_temp.assert_limits(app.complete_outbox(:'ta',:'final_event',:'worker',10),'CAP_FINAL_SUCCESS_ACK');
RESET ROLE;
\endif
\if :tenants
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(:'ta','limits.tenant','{"secret":"A"}','test','tenant'),(:'tb','limits.tenant','{"secret":"B"}','test','tenant');
SELECT set_config('app.tenant_id','',true);
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_limits((SELECT count(*)=2 FROM app.outbox_active_tenants(NULL,1000) WHERE tenant_id IN (:'ta',:'tb')),'TENANTS_DISCOVER_WITHOUT_SCOPE');
SELECT pg_temp.assert_limits((SELECT count(*)=1 FROM app.outbox_active_tenants(NULL,1)),'TENANTS_PAGE_LIMIT');
SELECT pg_temp.assert_limits((SELECT bool_and(tenant_id>least(:'ta'::uuid,:'tb'::uuid)) FROM app.outbox_active_tenants(least(:'ta'::uuid,:'tb'::uuid),1000)),'TENANTS_KEYSET');
SELECT pg_temp.reject_limits('SELECT app.outbox_active_tenants(NULL,0)','22023','TENANTS_INVALID_LIMIT');
SELECT pg_temp.reject_limits('SELECT payload FROM app.outbox_events','42501','TENANTS_NO_PAYLOAD');
SELECT pg_temp.reject_limits('SELECT tenant_id FROM app.outbox_events','42501','TENANTS_NO_DIRECT_IDS');
-- SET ROLE checks the original session_user, which is the privileged test
-- harness; inspect the worker's actual membership instead of its superuser host.
SELECT pg_temp.assert_limits(NOT pg_has_role('app_outbox_worker','app_outbox_tenant_broker','SET'),'TENANTS_NO_BROKER_MEMBERSHIP');
RESET ROLE;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.reject_limits('SELECT app.outbox_active_tenants(NULL,1000)','42501','TENANTS_RUNTIME_DENIED');
RESET ROLE;
SELECT pg_temp.assert_limits(NOT has_column_privilege('app_outbox_tenant_broker','app.outbox_events','payload','SELECT'),'TENANTS_BROKER_NO_PAYLOAD');
SELECT pg_temp.assert_limits((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='app.outbox_events'::regclass),'TENANTS_FORCE_RLS');
UPDATE app.outbox_events SET status='dead_letter' WHERE tenant_id=:'tb';
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_limits(NOT EXISTS(SELECT FROM app.outbox_active_tenants(NULL,1000) WHERE tenant_id=:'tb'),'TENANTS_DLQ_EXCLUDED');
RESET ROLE;
\endif
ROLLBACK;
\echo RELIABILITY_LIMITS_OK
