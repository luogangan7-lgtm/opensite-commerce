-- Queue capability boundary only. River owns its own schema/history; these
-- grants run after River migrations under the controlled installer identity.
-- Queue has no business tenant column: runtime row policy checks signed job
-- context while workers validate the proof before controlled app operations.
DO $$ BEGIN
 IF to_regclass('queue.river_job') IS NULL THEN RAISE EXCEPTION 'RIVER_INSTALL_REQUIRED'; END IF;
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_queue_worker') THEN CREATE ROLE app_queue_worker; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_queue_worker'::regrole)
 OR EXISTS(SELECT FROM pg_roles WHERE NOT rolcanlogin AND rolname<>'app_queue_worker'
 AND pg_has_role(oid,'app_queue_worker','MEMBER')) THEN
 RAISE EXCEPTION 'QUEUE_WORKER_MEMBERSHIP_UNSAFE'; END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE NOT rolcanlogin AND rolname<>'app_river_migrator'
 AND pg_has_role(oid,'app_river_migrator','MEMBER')) THEN
 RAISE EXCEPTION 'RIVER_MIGRATOR_UNSAFE'; END IF;
END $$;
ALTER ROLE app_queue_worker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_queue_worker;
GRANT EXECUTE ON FUNCTION app.inventory_expire_due(integer) TO app_queue_worker;

SET LOCAL ROLE app_river_migrator;
REVOKE ALL ON SCHEMA queue FROM PUBLIC,app_runtime,app_queue_worker;
GRANT USAGE ON SCHEMA queue TO app_runtime,app_queue_worker;
REVOKE ALL ON ALL TABLES IN SCHEMA queue FROM PUBLIC,app_runtime,app_queue_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA queue FROM PUBLIC,app_runtime,app_queue_worker;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA queue FROM PUBLIC,app_runtime,app_queue_worker;
GRANT SELECT ON queue.river_job TO app_runtime;
GRANT INSERT(id,args,created_at,kind,max_attempts,metadata,priority,queue,scheduled_at,state,tags,unique_key,unique_states)
 ON queue.river_job TO app_runtime;
GRANT UPDATE(kind) ON queue.river_job TO app_runtime;
GRANT USAGE ON SEQUENCE queue.river_job_id_seq TO app_runtime,app_queue_worker;
GRANT EXECUTE ON FUNCTION queue.river_job_state_in_bitmask(bit,queue.river_job_state) TO app_runtime,app_queue_worker;
GRANT SELECT,INSERT,UPDATE,DELETE ON queue.river_job,queue.river_queue,queue.river_leader TO app_queue_worker;
GRANT SELECT ON queue.river_migration TO app_queue_worker;
ALTER TABLE queue.river_job ENABLE ROW LEVEL SECURITY;
ALTER TABLE queue.river_job FORCE ROW LEVEL SECURITY;
CREATE POLICY runtime_tenant ON queue.river_job TO app_runtime
 USING (args->>'tenant_id'=nullif(current_setting('app.tenant_id',true),''))
 WITH CHECK (args->>'tenant_id'=nullif(current_setting('app.tenant_id',true),''));
CREATE POLICY trusted_queue_processing ON queue.river_job TO app_queue_worker,app_river_migrator
 USING (true) WITH CHECK (true);
RESET ROLE;
