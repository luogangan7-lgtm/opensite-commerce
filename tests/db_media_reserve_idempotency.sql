-- Responsibility: media's shared claim grants and tenant/operation isolation.
-- Dependencies: all forward migrations; callers: CI clean and upgrade gates.
-- Invariants: the media role cannot inspect another tenant/operation or delete
-- receipts; malformed caller keys still fail the existing UUID CHECK.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('media-idem-a') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('media-idem-b') RETURNING id AS other \gset
SELECT uuidv7() AS actor, uuidv7() AS key \gset
INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(:'t','user',:'actor','otherOperation',:'key',sha256('other'::bytea),clock_timestamp()+interval '1 minute',uuidv7()),
 (:'other','user',:'actor','createMediaUpload',:'key',sha256('other'::bytea),clock_timestamp()+interval '1 minute',uuidv7());
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_media_service;
DO $gate$ DECLARE n integer; BEGIN
 SELECT count(*) INTO n FROM app.idempotency_records;
 IF n<>0 THEN RAISE EXCEPTION 'MEDIA_IDEM_FOREIGN_RECEIPT_VISIBLE'; END IF;
 BEGIN
  PERFORM app.idempotency_claim(current_setting('app.tenant_id')::uuid,'user',uuidv7(),'otherOperation',uuidv7()::text,sha256('x'::bytea),interval '1 minute',uuidv7());
  RAISE EXCEPTION 'MEDIA_IDEM_OTHER_OPERATION_ADMITTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.idempotency_claim(current_setting('app.tenant_id')::uuid,'user',uuidv7(),'createMediaUpload','not-a-uuid',sha256('x'::bytea),interval '1 minute',uuidv7());
  RAISE EXCEPTION 'MEDIA_IDEM_UUID_CHECK_MISSING';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  DELETE FROM app.idempotency_records;
  RAISE EXCEPTION 'MEDIA_IDEM_DELETE_GRANTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $gate$;
SELECT (app.idempotency_claim(:'t','user',:'actor','createMediaUpload',:'key',sha256('reserve'::bytea),interval '1 minute',uuidv7())).claim_token AS token \gset
SELECT set_config('app.idempotency_claim',:'token',true);
UPDATE app.idempotency_records SET state='completed',response_status=201,
 response_body_reference='fixture-reference',completed_at=clock_timestamp()
 WHERE tenant_id=:'t' AND actor_id=:'actor' AND idempotency_key=:'key' AND claim_token=:'token';
DO $gate$ BEGIN
 IF (SELECT count(*) FROM app.idempotency_records WHERE state='completed' AND response_status=201)<>1 THEN
  RAISE EXCEPTION 'MEDIA_IDEM_COMPLETION_MISSING'; END IF;
END $gate$;
RESET ROLE;
ROLLBACK;
\echo 'PASS: media reserve shared claim, fenced completion, tenant/operation isolation and UUID CHECK'
