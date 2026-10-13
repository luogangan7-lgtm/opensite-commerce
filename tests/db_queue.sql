-- Responsibility: prove native producer ACL/RLS and worker command isolation.
-- Dependencies: pinned River queue + queue authorization; privileged test runner.
-- Caller: check_pg/upgrade on expendable PG18; fixtures rollback, no business effects.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('queue-gate-A') RETURNING id AS tenant_a \gset
INSERT INTO app.tenants(name) VALUES('queue-gate-B') RETURNING id AS tenant_b \gset
SELECT set_config('queuegate.tenant_a',:'tenant_a',true),set_config('queuegate.tenant_b',:'tenant_b',true);
-- Positive witness must exist; an empty queue may not prove isolation.
INSERT INTO queue.river_job(args,kind,queue) VALUES(jsonb_build_object('tenant_id',:'tenant_b'),'queue_gate','default');
SET LOCAL ROLE app_runtime;
SELECT set_config('app.tenant_id',:'tenant_a',true);
DO $producer$
DECLARE a text:=current_setting('queuegate.tenant_a'); b text:=current_setting('queuegate.tenant_b'); j bigint; repeated bigint; inserted queue.river_job%ROWTYPE;
BEGIN
 IF (SELECT count(*) FROM queue.river_job WHERE args->>'tenant_id'=b)<>0 THEN
  RAISE EXCEPTION 'QUEUE_CROSS_TENANT_READ'; END IF;
 -- This is the native River0.47 insert shape: RETURNING all columns, conflict
 -- UPDATE(kind), sequence and state helper permissions are all required.
 INSERT INTO queue.river_job(id,args,created_at,kind,max_attempts,metadata,priority,queue,scheduled_at,state,tags,unique_key,unique_states)
 VALUES(nextval('queue.river_job_id_seq'),jsonb_build_object('tenant_id',a),now(),'queue_gate',25,'{}',1,'default',now(),'available','{}',sha256(convert_to(a,'UTF8')),B'11111111')
 ON CONFLICT(unique_key) WHERE unique_key IS NOT NULL AND unique_states IS NOT NULL AND queue.river_job_state_in_bitmask(unique_states,state)
 DO UPDATE SET kind=EXCLUDED.kind RETURNING * INTO inserted;
 j:=inserted.id;
 INSERT INTO queue.river_job(id,args,created_at,kind,max_attempts,metadata,priority,queue,scheduled_at,state,tags,unique_key,unique_states)
 VALUES(nextval('queue.river_job_id_seq'),jsonb_build_object('tenant_id',a),now(),'queue_gate',25,'{}',1,'default',now(),'available','{}',sha256(convert_to(a,'UTF8')),B'11111111')
 ON CONFLICT(unique_key) WHERE unique_key IS NOT NULL AND unique_states IS NOT NULL AND queue.river_job_state_in_bitmask(unique_states,state)
 DO UPDATE SET kind=EXCLUDED.kind RETURNING id INTO repeated;
 IF j IS NULL OR repeated IS DISTINCT FROM j OR (SELECT count(*) FROM queue.river_job WHERE id=j)<>1 THEN
  RAISE EXCEPTION 'QUEUE_NATIVE_REPLAY_INVALID'; END IF;
 PERFORM set_config('app.tenant_id',b,true);
 BEGIN
  INSERT INTO queue.river_job(args,kind,unique_key,unique_states)
  VALUES(jsonb_build_object('tenant_id',b),'queue_gate',sha256(convert_to(a,'UTF8')),B'11111111')
  ON CONFLICT(unique_key) WHERE unique_key IS NOT NULL AND unique_states IS NOT NULL AND queue.river_job_state_in_bitmask(unique_states,state)
  DO UPDATE SET kind=EXCLUDED.kind;
  RAISE EXCEPTION 'QUEUE_CROSS_TENANT_CONFLICT_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',a,true);
 BEGIN
  INSERT INTO queue.river_job(args,kind) VALUES(jsonb_build_object('tenant_id',b),'queue_gate');
  RAISE EXCEPTION 'QUEUE_CROSS_TENANT_INSERT_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE queue.river_job SET args=jsonb_build_object('tenant_id',b) WHERE id=j;
  RAISE EXCEPTION 'QUEUE_ARGS_UPDATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  DELETE FROM queue.river_job WHERE id=j; RAISE EXCEPTION 'QUEUE_RUNTIME_DELETE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  TRUNCATE queue.river_job; RAISE EXCEPTION 'QUEUE_RUNTIME_TRUNCATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  CREATE TABLE queue.queuegate_forbidden(id int); RAISE EXCEPTION 'QUEUE_RUNTIME_CREATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  ALTER TABLE queue.river_job ADD COLUMN queuegate_forbidden int; RAISE EXCEPTION 'QUEUE_RUNTIME_ALTER_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM queue.river_job) THEN RAISE EXCEPTION 'QUEUE_UNSCOPED_READ'; END IF;
 BEGIN
  INSERT INTO queue.river_job(args,kind) VALUES(jsonb_build_object('tenant_id',a),'queue_gate');
  RAISE EXCEPTION 'QUEUE_UNSCOPED_INSERT_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $producer$;
RESET ROLE;
-- The worker sees both tenants for queue processing; it receives no general
-- business table access. Signature admission is separately tested through Go.
SET LOCAL ROLE app_queue_worker;
DO $worker$
BEGIN
 IF (SELECT count(*) FROM queue.river_job WHERE kind='queue_gate' AND args->>'tenant_id' IN
  (current_setting('queuegate.tenant_a'),current_setting('queuegate.tenant_b')))<>2 THEN
  RAISE EXCEPTION 'QUEUE_WORKER_VISIBILITY_INVALID'; END IF;
 BEGIN
  PERFORM count(*) FROM app.inventory_reservations;
  RAISE EXCEPTION 'QUEUE_WORKER_BUSINESS_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.stock_levels SET reserved=0;
  RAISE EXCEPTION 'QUEUE_WORKER_BUSINESS_WRITE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  CREATE TABLE queue.queuegate_forbidden(id int); RAISE EXCEPTION 'QUEUE_WORKER_CREATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  TRUNCATE queue.river_job; RAISE EXCEPTION 'QUEUE_WORKER_TRUNCATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $worker$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: queue native replay, scoped producer and worker business/DDL denial' AS result;
