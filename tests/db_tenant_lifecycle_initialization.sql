-- Responsibility: new tenant initialization must agree with populated backfill.
-- Dependencies: all migrations and CI disposable PG18; called by check_pg.sh.
-- Invariant: an already-deleted tenant never acquires active write admission.
\set ON_ERROR_STOP on
BEGIN;
-- Frozen 001 currently admits active/suspended only. Extend the disposable
-- fixture to the already-designed tombstone input; never alter frozen DDL or
-- disable its current checks in production. This extension rolls back below.
DO $fixture$
DECLARE original text;
BEGIN
 SELECT pg_get_expr(conbin,conrelid) INTO STRICT original FROM pg_constraint
 WHERE conrelid='app.tenants'::regclass AND conname='tenants_status_check';
 ALTER TABLE app.tenants DROP CONSTRAINT tenants_status_check;
 EXECUTE 'ALTER TABLE app.tenants ADD CONSTRAINT tenants_status_check CHECK (('||original||') OR status=''deleted'')';
END $fixture$;
DO $test$
DECLARE t uuid; l app.tenant_lifecycles; before_db timestamptz:=clock_timestamp();
BEGIN
 INSERT INTO app.tenants(name,status) VALUES('mdel01-already-deleted','deleted') RETURNING id INTO t;
 SELECT * INTO STRICT l FROM app.tenant_lifecycles WHERE tenant_id=t AND id=t;
 IF l.state<>'deleted' OR l.completed_at IS NULL OR l.completed_at<before_db OR l.completed_at>clock_timestamp() THEN
  RAISE EXCEPTION 'MDEL01_DELETED_INITIALIZATION: state=%, completed_at=%',l.state,l.completed_at;
 END IF;
 PERFORM set_config('app.tenant_id',t::text,true);
 BEGIN
  PERFORM app.tenant_lifecycle_write_fence(t);
  RAISE EXCEPTION 'MDEL01_DELETED_INITIAL_WRITE';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 INSERT INTO app.tenants(name) VALUES('mdel01-new-active') RETURNING id INTO t;
 SELECT * INTO STRICT l FROM app.tenant_lifecycles WHERE tenant_id=t AND id=t;
 IF l.state<>'active' OR l.completed_at IS NOT NULL THEN RAISE EXCEPTION 'MDEL01_ACTIVE_INITIALIZATION'; END IF;
END $test$;
ROLLBACK;
SELECT 'PASS: MDEL01 deleted and active tenant initialization';
