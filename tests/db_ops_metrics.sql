-- Responsibility: aggregate-only worker metrics and exact denial witnesses.
-- Dependencies: forward migrations; caller: disposable check_pg.sh harness.
-- Invariants: cross-tenant backlog/DLQ counts expose no event IDs or payload,
-- application roles cannot scrape; the broker cannot read event content or write.
\set ON_ERROR_STOP on
BEGIN;
CREATE FUNCTION pg_temp.assert_ops_metrics(ok boolean,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 IF ok IS DISTINCT FROM true THEN RAISE EXCEPTION 'OPS_METRICS_GATE %',label; END IF;
END $$;
CREATE FUNCTION pg_temp.reject_ops_metrics(q text,expected text,label text) RETURNS void
LANGUAGE plpgsql AS $$ DECLARE got text; BEGIN
 BEGIN EXECUTE q; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS got=RETURNED_SQLSTATE; END;
 IF got IS DISTINCT FROM expected THEN
  RAISE EXCEPTION 'OPS_METRICS_GATE % expected %, got %',label,expected,coalesce(got,'success');
 END IF;
END $$;
-- Scrape cost is bounded by the backlog, not history: with sequential scans
-- disabled the gauge must come from the backlog partial index (zero seq scans).
-- Must be this session's first call: PG18 caches SQL-function plans per session.
SELECT pg_stat_get_xact_numscans('app.outbox_events'::regclass) AS seq_before \gset
SET LOCAL enable_seqscan=off;
SELECT count(*) FROM app.outbox_metrics() \gset discard_
RESET enable_seqscan;
SELECT pg_temp.assert_ops_metrics(pg_stat_get_xact_numscans('app.outbox_events'::regclass)=:'seq_before'::bigint,'SCRAPE_WITHOUT_SEQUENTIAL_SCAN');
-- Missing capability is deliberately fatal before any data fixture is written.
SELECT pending AS baseline_pending,dead_letters AS baseline_dead FROM app.outbox_metrics() \gset
INSERT INTO app.tenants(name) VALUES('ops-metrics-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('ops-metrics-DLQ-only') RETURNING id AS tb \gset
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key,status,created_at,lease_owner,lease_until,attempts,delivered_at)
 VALUES
 (:'ta','ops.metrics','{"secret":"A"}','test','pending','pending',clock_timestamp()-interval '10 minutes',NULL,NULL,0,NULL),
 (:'ta','ops.metrics','{"secret":"B"}','test','failed','failed',clock_timestamp()-interval '5 minutes',NULL,NULL,1,NULL),
 (:'ta','ops.metrics','{"secret":"C"}','test','processing','processing',clock_timestamp()-interval '20 minutes',uuidv7(),clock_timestamp()+interval '1 minute',1,NULL),
 (:'ta','ops.metrics','{"secret":"D"}','test','delivered','delivered',clock_timestamp()-interval '1 hour',NULL,NULL,1,clock_timestamp()),
 (:'tb','ops.metrics','{"secret":"E"}','test','dead','dead_letter',clock_timestamp()-interval '1 hour',NULL,NULL,10,NULL);
SELECT set_config('app.tenant_id','',true);
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_ops_metrics((SELECT pending=:'baseline_pending'::bigint+3 AND dead_letters=:'baseline_dead'::bigint+1
 AND oldest_pending_age_seconds>=1200 FROM app.outbox_metrics()),'COUNTS_INCLUDE_DLQ_ONLY_TENANT');
SELECT pg_temp.assert_ops_metrics((SELECT count(*)=1 FROM app.outbox_metrics()),'ONE_AGGREGATE_ROW');
SELECT pg_temp.reject_ops_metrics('SELECT tenant_id FROM app.outbox_metrics()','42703','NO_TENANT_ID_OUTPUT');
SELECT pg_temp.reject_ops_metrics('SELECT payload FROM app.outbox_events','42501','WORKER_NO_DIRECT_PAYLOAD');
SELECT pg_temp.reject_ops_metrics('SELECT created_at FROM app.outbox_events','42501','WORKER_NO_DIRECT_TIMESTAMPS');
SELECT pg_temp.reject_ops_metrics('SELECT tenant_id FROM app.outbox_events','42501','WORKER_NO_DIRECT_IDS');
SELECT pg_temp.reject_ops_metrics('CREATE TABLE app.ops_metrics_escape(id integer)','42501','WORKER_NO_DDL');
RESET ROLE;
-- Clock skew cannot emit a negative gauge; delivered/DLQ ages do not affect it.
UPDATE app.outbox_events SET created_at=clock_timestamp()+interval '1 hour' WHERE status IN('pending','processing','failed');
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_ops_metrics((SELECT oldest_pending_age_seconds=0 FROM app.outbox_metrics()),'FUTURE_CREATED_AT_CLAMP');
RESET ROLE;
UPDATE app.outbox_events SET status='delivered',lease_owner=NULL,lease_until=NULL,delivered_at=clock_timestamp() WHERE status IN('pending','processing','failed');
SET LOCAL ROLE app_outbox_worker;
SELECT pg_temp.assert_ops_metrics((SELECT pending=0 AND oldest_pending_age_seconds=0 AND dead_letters=:'baseline_dead'::bigint+1
 FROM app.outbox_metrics()),'EMPTY_BACKLOG_ZERO_AGE');
RESET ROLE;
DO $$ DECLARE role_name text; BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_runtime','app_auth','app_domain_resolver','app_queue_worker','app_media_service','app_publisher'] LOOP
  EXECUTE format('SET LOCAL ROLE %I',role_name);
  PERFORM pg_temp.reject_ops_metrics('SELECT app.outbox_metrics()','42501','EXECUTE_DENIED_'||role_name);
  RESET ROLE;
 END LOOP;
END $$;
SET LOCAL ROLE app_outbox_tenant_broker;
SELECT pg_temp.reject_ops_metrics('SELECT payload FROM app.outbox_events','42501','BROKER_NO_PAYLOAD');
SELECT pg_temp.reject_ops_metrics('SELECT id FROM app.outbox_events','42501','BROKER_NO_EVENT_ID');
SELECT pg_temp.reject_ops_metrics('UPDATE app.outbox_events SET status=''delivered''','42501','BROKER_NO_WRITE');
RESET ROLE;
SELECT pg_temp.assert_ops_metrics(NOT pg_has_role('app_outbox_worker','app_outbox_tenant_broker','SET'),'NO_BROKER_MEMBERSHIP');
SELECT pg_temp.assert_ops_metrics((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='app.outbox_events'::regclass),'FORCE_RLS_RETAINED');
SELECT pg_temp.assert_ops_metrics((SELECT prosecdef AND proowner='app_outbox_tenant_broker'::regrole
 AND proconfig=ARRAY['search_path=pg_catalog, app'] AND proargnames=ARRAY['pending','dead_letters','oldest_pending_age_seconds']
 AND proallargtypes=ARRAY['bigint'::regtype,'bigint'::regtype,'double precision'::regtype]::oid[]
 FROM pg_proc WHERE oid='app.outbox_metrics()'::regprocedure),'EXACT_AGGREGATE_SIGNATURE_OWNER_PATH');
ROLLBACK;
\echo OPS_METRICS_OK
