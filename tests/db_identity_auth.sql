-- Responsibility: executable global identity/credential capability contract.
-- Dependencies: all forward migrations through *_identity_auth.sql; PG18.
-- Caller: check_pg.sh and populated upgrade gate, privileged disposable DB.
-- Invariants: failures are atomic/bounded; revoked devices never revive;
-- bootstrap sees owners across tenants; ordinary roles cannot access credentials.
\set ON_ERROR_STOP on
BEGIN;
-- INT22_CLOCK_PROBE_BEGIN: production callable overloads accept no caller clock.
DO $clock_probe$
BEGIN
 IF to_regprocedure('app.auth_record_failure(text,inet,timestamptz)') IS NOT NULL OR
    to_regprocedure('app.auth_fail_known_device(uuid,timestamptz)') IS NOT NULL THEN
  RAISE EXCEPTION 'INT22_CLOCK caller-supplied clock overload remains'; END IF;
 IF to_regprocedure('app.auth_record_failure(text,inet)') IS NULL OR
    to_regprocedure('app.auth_fail_known_device(uuid)') IS NULL THEN
  RAISE EXCEPTION 'INT22_CLOCK server-clock production signatures absent'; END IF;
END $clock_probe$;
-- INT22_CLOCK_PROBE_END
-- INT22_RETENTION_PROBE_BEGIN: bounded cleanup is execute-only to app_auth.
DO $retention_probe$
BEGIN
 IF to_regprocedure('app.auth_prune_attempts(integer)') IS NULL THEN
  RAISE EXCEPTION 'INT22_RETENTION cleanup capability absent'; END IF;
 IF NOT has_function_privilege('app_auth','app.auth_prune_attempts(integer)','EXECUTE') OR
    has_table_privilege('app_auth','app.auth_account_attempts','DELETE') OR
    has_table_privilege('app_auth','app.auth_login_attempts','DELETE') THEN
  RAISE EXCEPTION 'INT22_RETENTION cleanup capability boundary'; END IF;
END $retention_probe$;
-- INT22_RETENTION_PROBE_END
-- INT22_TENANT_PROBE_BEGIN: successful bootstrap restores caller transaction GUC.
-- Privileged disposable-DB test only: isolate the success branch from upgrade
-- owners, then immediately restore every existing owner and the caller GUC.
SAVEPOINT int22_tenant_probe;
DELETE FROM app.tenant_memberships WHERE role='owner';
DO $tenant_probe$
DECLARE before text; created uuid; value text;
BEGIN
 FOREACH value IN ARRAY ARRAY['','aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'] LOOP
  PERFORM set_config('app.tenant_id',value,true);
  before:=current_setting('app.tenant_id',true);
  created:=app.bootstrap_admin('int22-'||coalesce(nullif(value,''),'empty')||'@example.test','INT22 probe',
   '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
  IF current_setting('app.tenant_id',true) IS DISTINCT FROM before THEN
   RAISE EXCEPTION 'INT22_TENANT bootstrap leaked caller tenant'; END IF;
  IF (SELECT count(*) FROM app.buyer_roles WHERE tenant_id=created)<>3 THEN
   RAISE EXCEPTION 'INT22_TENANT bootstrap missed buyer role vocabulary'; END IF;
  DELETE FROM app.tenant_memberships WHERE tenant_id=created;
  DELETE FROM app.tenants WHERE id=created;
  DELETE FROM app.users WHERE email LIKE 'int22-%@example.test';
 END LOOP;
END $tenant_probe$;
ROLLBACK TO SAVEPOINT int22_tenant_probe;
RELEASE SAVEPOINT int22_tenant_probe;
-- INT22_TENANT_PROBE_END

INSERT INTO app.users(email,password_hash) VALUES('c02a-device@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.tenants(name) VALUES('c02a-key-a') RETURNING id AS t1 \gset
INSERT INTO app.tenants(name) VALUES('c02a-key-b') RETURNING id AS t2 \gset
SELECT set_config('test.identity_user',:'u',true);
SELECT set_config('test.identity_tenant',:'t1',true);
INSERT INTO app.known_devices(user_id,cookie_hash,expires_at,created_at)
 VALUES(:'u',sha256('c02a-cookie'::bytea),now()+interval '1 day',now()) RETURNING id AS device \gset
SELECT set_config('test.identity_device',:'device',true);
SET LOCAL ROLE app_auth;
DO $counter$
DECLARE t timestamptz; before_call timestamptz; after_call timestamptz;
 a app.auth_account_attempts; ip app.auth_login_attempts; i int;
 first_cooldown timestamptz; ip_cooldown timestamptz;
BEGIN
 -- Bound each server sample by DB wall time; transaction now() is not authority.
 before_call:=clock_timestamp();
 a:=app.auth_record_failure('  C02A-MISSING@example.test  ','192.0.2.1');
 after_call:=clock_timestamp();
 first_cooldown:=a.cooldown_until;
 IF a.failure_count IS DISTINCT FROM 1 OR
  (a.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
  a.cooldown_until-a.last_failed_at IS DISTINCT FROM interval '1 second' THEN
  RAISE EXCEPTION 'IDENTITY_GATE first failure/server-clock/full backoff'; END IF;
 before_call:=clock_timestamp();
 a:=app.auth_record_failure('c02a-missing@example.test','192.0.2.1');
 after_call:=clock_timestamp();
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized;
 IF a.failure_count IS DISTINCT FROM 2 OR ip.failure_count IS DISTINCT FROM 2 OR
  a.cooldown_until IS DISTINCT FROM first_cooldown OR
  (a.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
  ip.last_failed_at IS DISTINCT FROM a.last_failed_at THEN
  RAISE EXCEPTION 'IDENTITY_GATE atomic two failures/server-clock/non-sliding cooldown'; END IF;
 IF EXISTS(SELECT FROM app.users WHERE email_normalized=a.identifier_normalized) THEN
  RAISE EXCEPTION 'IDENTITY_GATE missing account fixture unexpectedly exists'; END IF;
 -- Advance state, never the production callable clock.
 UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized=a.identifier_normalized;
 before_call:=clock_timestamp();
 a:=app.auth_record_failure('c02a-missing@example.test','192.0.2.1');
 after_call:=clock_timestamp();
 IF a.failure_count IS DISTINCT FROM 3 OR
  (a.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
  a.cooldown_until-a.last_failed_at IS DISTINCT FROM interval '4 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE progressive backoff'; END IF;
 FOR i IN 1..9 LOOP
  UPDATE app.auth_account_attempts SET cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized='c02a-cap@example.test';
  before_call:=clock_timestamp();
  a:=app.auth_record_failure('c02a-cap@example.test','192.0.2.2');
  after_call:=clock_timestamp();
  IF a.failure_count IS DISTINCT FROM i OR
   (a.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
   a.cooldown_until-a.last_failed_at IS DISTINCT FROM least(60,1 << least(6,i-1))*interval '1 second' THEN
   RAISE EXCEPTION 'IDENTITY_GATE backoff cap/progression'; END IF;
 END LOOP;
 IF a.cooldown_until-a.last_failed_at IS DISTINCT FROM interval '60 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE cap reached'; END IF;
 UPDATE app.auth_account_attempts SET failure_count=1000000,cooldown_until=clock_timestamp()-interval '1 second' WHERE identifier_normalized=a.identifier_normalized;
 before_call:=clock_timestamp();
 a:=app.auth_record_failure(a.identifier_normalized,'192.0.2.2');
 after_call:=clock_timestamp();
 IF a.failure_count IS DISTINCT FROM 1000000 OR
  (a.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
  a.cooldown_until-a.last_failed_at IS DISTINCT FROM interval '60 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE saturating count/backoff'; END IF;
 FOR i IN 1..6 LOOP
  before_call:=clock_timestamp();
  a:=app.auth_record_failure('c02a-ip@example.test','192.0.2.3');
  after_call:=clock_timestamp();
  SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized;
  IF ip.failure_count IS DISTINCT FROM i OR
   (ip.last_failed_at BETWEEN before_call AND after_call) IS NOT TRUE OR
   ip.last_failed_at IS DISTINCT FROM a.last_failed_at THEN
   RAISE EXCEPTION 'IDENTITY_GATE atomic IP accounting/server-clock'; END IF;
  IF i<=5 AND ip.cooldown_until-ip.last_failed_at IS DISTINCT FROM
   (CASE WHEN i<5 THEN interval '0 seconds' ELSE interval '60 seconds' END) THEN
   RAISE EXCEPTION 'IDENTITY_GATE IP threshold/full backoff'; END IF;
  IF i=5 THEN ip_cooldown:=ip.cooldown_until; END IF;
 END LOOP;
 IF ip.failure_count IS DISTINCT FROM 6 OR ip.cooldown_until IS DISTINCT FROM ip_cooldown THEN
  RAISE EXCEPTION 'IDENTITY_GATE IP lockout must not slide'; END IF;
 t:=clock_timestamp();
 UPDATE app.auth_account_attempts SET window_started_at=t-interval '16 minutes',cooldown_until=t-interval '1 second' WHERE identifier_normalized=ip.identifier_normalized;
 UPDATE app.auth_login_attempts SET window_started_at=t-interval '16 minutes',cooldown_until=t-interval '1 second' WHERE identifier_normalized=ip.identifier_normalized;
 a:=app.auth_record_failure('c02a-ip@example.test','192.0.2.3');
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized;
 IF a.failure_count<>1 OR ip.failure_count<>1 THEN RAISE EXCEPTION 'IDENTITY_GATE counter window reset'; END IF;
 -- Crossing the counting window cannot shorten an already-active cooldown.
 PERFORM app.auth_record_failure('c02a-window@example.test','192.0.2.5');
 t:=clock_timestamp();
 UPDATE app.auth_account_attempts SET window_started_at=t-interval '16 minutes',cooldown_until=t+interval '60 seconds' WHERE identifier_normalized='c02a-window@example.test';
 UPDATE app.auth_login_attempts SET window_started_at=t-interval '16 minutes',cooldown_until=t+interval '60 seconds' WHERE identifier_normalized='c02a-window@example.test';
 a:=app.auth_record_failure('c02a-window@example.test','192.0.2.5');
 IF a.failure_count<>1 OR a.cooldown_until<>t+interval '60 seconds' OR
  (SELECT cooldown_until FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized)<>t+interval '60 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE window reset changed active cooldown'; END IF;
 BEGIN PERFORM app.auth_record_failure('','192.0.2.4'); RAISE EXCEPTION 'IDENTITY_GATE empty identifier';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.auth_record_failure('x','192.0.2.0/24'); RAISE EXCEPTION 'IDENTITY_GATE network is not an IP';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
END $counter$;
DO $retention$
DECLARE r record; i int; t timestamptz:=now();
BEGIN
 -- Batch-1 removes at most one old IP and one now-empty old account.
 INSERT INTO app.auth_account_attempts(identifier_normalized,window_started_at,last_failed_at)
 SELECT 'int22-stale-'||n,t-interval '2 days',t-interval '2 days' FROM generate_series(1,3) n;
 INSERT INTO app.auth_login_attempts(identifier_normalized,ip,window_started_at,last_failed_at)
 SELECT 'int22-stale-'||n,'192.0.2.100'::inet,t-interval '2 days',t-interval '2 days' FROM generate_series(1,3) n;
 INSERT INTO app.auth_account_attempts(identifier_normalized,window_started_at,last_failed_at,cooldown_until) VALUES
 ('int22-live',t,t,t+interval '60 seconds'),
 ('int22-cooling',t-interval '2 days',t-interval '2 days',t+interval '60 seconds'),
 ('int22-boundary',t-interval '24 hours',t-interval '24 hours','-infinity');
 INSERT INTO app.auth_login_attempts(identifier_normalized,ip,window_started_at,last_failed_at,cooldown_until) VALUES
 ('int22-live','192.0.2.101',t-interval '2 days',t-interval '2 days','-infinity'),
 ('int22-live','192.0.2.102',t,t,t+interval '60 seconds'),
 ('int22-cooling','192.0.2.103',t-interval '2 days',t-interval '2 days',t+interval '60 seconds');
 SELECT * INTO STRICT r FROM app.auth_prune_attempts(1);
 IF r.accounts_deleted NOT BETWEEN 0 AND 1 OR r.ips_deleted<>1 THEN
  RAISE EXCEPTION 'INT22_RETENTION per-table batch cap'; END IF;
 FOR i IN 1..4 LOOP PERFORM app.auth_prune_attempts(); END LOOP;
 IF EXISTS(SELECT FROM app.auth_account_attempts WHERE identifier_normalized LIKE 'int22-stale-%') OR
 EXISTS(SELECT FROM app.auth_login_attempts WHERE identifier_normalized='int22-live' AND ip='192.0.2.101') THEN
  RAISE EXCEPTION 'INT22_RETENTION expired records retained'; END IF;
 IF (SELECT count(*) FROM app.auth_account_attempts WHERE identifier_normalized=ANY(ARRAY['int22-live','int22-cooling','int22-boundary']))<>3 OR
 (SELECT count(*) FROM app.auth_login_attempts WHERE identifier_normalized IN ('int22-live','int22-cooling'))<>2 THEN
  RAISE EXCEPTION 'INT22_RETENTION active/boundary records deleted'; END IF;
 SELECT * INTO STRICT r FROM app.auth_prune_attempts();
 IF r.accounts_deleted<>0 OR r.ips_deleted<>0 THEN RAISE EXCEPTION 'INT22_RETENTION not idempotent'; END IF;
 BEGIN PERFORM app.auth_prune_attempts(NULL); RAISE EXCEPTION 'INT22_RETENTION NULL batch accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.auth_prune_attempts(0); RAISE EXCEPTION 'INT22_RETENTION zero batch accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.auth_prune_attempts(1001); RAISE EXCEPTION 'INT22_RETENTION excessive batch accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN DELETE FROM app.auth_login_attempts WHERE identifier_normalized='int22-live';
  RAISE EXCEPTION 'INT22_RETENTION app_auth direct delete'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $retention$;
DO $device$
DECLARE d uuid:=current_setting('test.identity_device')::uuid; row app.known_devices; i int;
BEGIN
 FOR i IN 1..5 LOOP row:=app.auth_fail_known_device(d); END LOOP;
 IF row.failure_count<>5 OR row.revoked_at IS NULL THEN RAISE EXCEPTION 'IDENTITY_GATE device five-failure revocation'; END IF;
 row:=app.auth_fail_known_device(d);
 IF row.id IS NOT NULL THEN RAISE EXCEPTION 'IDENTITY_GATE revoked device reused'; END IF;
 BEGIN UPDATE app.known_devices SET revoked_at=NULL WHERE id=d; RAISE EXCEPTION 'IDENTITY_GATE device resurrected';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.known_devices SET failure_count=0 WHERE id=d; RAISE EXCEPTION 'IDENTITY_GATE device failures reset';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN INSERT INTO app.known_devices(user_id,cookie_hash,expires_at)
  VALUES(uuidv7(),sha256('c02a-orphan'::bytea),'2040-01-10Z'); RAISE EXCEPTION 'IDENTITY_GATE orphan device';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN INSERT INTO app.known_devices(user_id,cookie_hash,expires_at)
  VALUES(current_setting('test.identity_user')::uuid,sha256('c02a-cookie'::bytea),'2040-01-10Z');
  RAISE EXCEPTION 'IDENTITY_GATE duplicate device hash'; EXCEPTION WHEN unique_violation THEN NULL; END;
END $device$;
RESET ROLE;
-- A public API key identifier locates exactly one tenant even before auth knows it.
INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash)
 VALUES(:'t1','aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',:'u','test-a',sha256('c02a-key-a'::bytea));
DO $key$
DECLARE constraint_name text;
BEGIN
 BEGIN INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash)
  VALUES((SELECT id FROM app.tenants WHERE name='c02a-key-b'),'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
   current_setting('test.identity_user')::uuid,'test-b',sha256('c02a-key-b'::bytea));
  RAISE EXCEPTION 'IDENTITY_GATE public key id ambiguous across tenants';
 EXCEPTION WHEN unique_violation THEN GET STACKED DIAGNOSTICS constraint_name=CONSTRAINT_NAME;
  IF constraint_name<>'api_keys_public_id_unique' THEN RAISE EXCEPTION 'IDENTITY_GATE wrong unique constraint %',constraint_name; END IF;
 END;
END $key$;
SET LOCAL ROLE app_runtime;
DO $runtime$
BEGIN
 BEGIN PERFORM password_hash FROM app.users; RAISE EXCEPTION 'IDENTITY_GATE runtime password read';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN INSERT INTO app.auth_login_attempts(identifier_normalized,ip) VALUES('x','192.0.2.1');
  RAISE EXCEPTION 'IDENTITY_GATE runtime counter write'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM app.auth_prune_attempts(); RAISE EXCEPTION 'INT22_RETENTION runtime cleanup';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM app.bootstrap_admin('x@example.test','X','$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
  RAISE EXCEPTION 'IDENTITY_GATE runtime bootstrap'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $runtime$;
RESET ROLE;
SET LOCAL ROLE app_credential_writer;
DO $writer$
BEGIN
 BEGIN PERFORM id FROM app.products; RAISE EXCEPTION 'IDENTITY_GATE credential writer catalog read';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM password_hash FROM app.users; RAISE EXCEPTION 'IDENTITY_GATE credential writer password read';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM failure_count FROM app.auth_account_attempts; RAISE EXCEPTION 'IDENTITY_GATE writer counter read';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $writer$;
RESET ROLE;
-- Owner witness resides in another tenant, with no caller tenant GUC visibility.
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t2',:'u','owner');
SELECT set_config('app.tenant_id',:'t1',true);
SET LOCAL ROLE app_admin_bootstrap;
DO $bootstrap$
BEGIN
 BEGIN PERFORM app.bootstrap_admin('c02a-new@example.test','new tenant',
  '$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
  RAISE EXCEPTION 'IDENTITY_GATE another tenant owner ignored';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'BOOTSTRAP_OWNER_EXISTS' THEN RAISE; END IF;
 END;
END $bootstrap$;
RESET ROLE;
DO $rollback$
BEGIN
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM current_setting('test.identity_tenant') THEN
  RAISE EXCEPTION 'INT22_TENANT rejected bootstrap leaked caller tenant'; END IF;
 IF EXISTS(SELECT FROM app.users WHERE email='c02a-new@example.test') OR
 EXISTS(SELECT FROM app.tenants WHERE name='new tenant') THEN RAISE EXCEPTION 'IDENTITY_GATE refused bootstrap wrote data'; END IF;
END $rollback$;
ROLLBACK;
SELECT 'PASS: global auth server clock, bounded retention, non-sliding backoff, device revocation, tenant restoration and real-role denial' AS result;
