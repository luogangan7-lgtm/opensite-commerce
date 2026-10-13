-- Responsibility: aggregate-only outbox gauges for the isolated worker monitor.
-- Dependencies: reliability limits and its restricted broker; caller: forward
-- migrator and worker health/metrics server. Invariants: no IDs or payload in
-- output, no application EXECUTE, no worker table access or DDL, FORCE RLS kept.
DO $$ DECLARE r record; BEGIN
 SELECT * INTO STRICT r FROM pg_roles WHERE rolname='app_outbox_tenant_broker';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid) THEN
  RAISE EXCEPTION 'OUTBOX_METRICS_BROKER_UNSAFE' USING ERRCODE='55000';
 END IF;
END $$;
-- The existing SELECT-only broker policy spans tenants. Add only the timestamp
-- column needed for age; the worker receives the function, never these columns.
GRANT SELECT(created_at) ON app.outbox_events TO app_outbox_tenant_broker;
-- Scrapes read only the backlog partial index, so cost tracks pending+DLQ rows,
-- never the delivered history.
CREATE INDEX outbox_metrics_backlog ON app.outbox_events(status,created_at)
 WHERE status IN('pending','processing','failed','dead_letter');
CREATE FUNCTION app.outbox_metrics()
RETURNS TABLE(pending bigint,dead_letters bigint,oldest_pending_age_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,app AS $$
 SELECT (SELECT count(*) FROM app.outbox_events WHERE status IN('pending','processing','failed')),
        (SELECT count(*) FROM app.outbox_events WHERE status='dead_letter'),
        greatest(coalesce(extract(epoch FROM (statement_timestamp()-
          (SELECT min(created_at) FROM app.outbox_events WHERE status IN('pending','processing','failed')))),0),0)::double precision
$$;
GRANT CREATE ON SCHEMA app TO app_outbox_tenant_broker;
ALTER FUNCTION app.outbox_metrics() OWNER TO app_outbox_tenant_broker;
REVOKE CREATE ON SCHEMA app FROM app_outbox_tenant_broker;
REVOKE ALL ON FUNCTION app.outbox_metrics() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.outbox_metrics() TO app_outbox_worker;
