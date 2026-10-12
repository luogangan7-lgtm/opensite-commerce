-- Responsibility: executable global identity/credential capability contract.
-- Dependencies: all forward migrations through *_identity_auth.sql; PG18.
-- Caller: check_pg.sh and populated upgrade gate, privileged disposable DB.
-- Invariants: failures are atomic/bounded; revoked devices never revive;
-- bootstrap sees owners across tenants; ordinary roles cannot access credentials.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.users(email,password_hash) VALUES('c02a-device@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.tenants(name) VALUES('c02a-key-a') RETURNING id AS t1 \gset
INSERT INTO app.tenants(name) VALUES('c02a-key-b') RETURNING id AS t2 \gset
SELECT set_config('test.identity_user',:'u',true);
SELECT set_config('test.identity_tenant',:'t1',true);
INSERT INTO app.known_devices(user_id,cookie_hash,expires_at,created_at)
 VALUES(:'u',sha256('c02a-cookie'::bytea),'2040-01-10Z','2040-01-01Z') RETURNING id AS device \gset
SELECT set_config('test.identity_device',:'device',true);
SET LOCAL ROLE app_auth;
DO $counter$
DECLARE t timestamptz:='2040-01-01Z'; a app.auth_account_attempts; ip app.auth_login_attempts; i int;
BEGIN
 a:=app.auth_record_failure('  C02A-MISSING@example.test  ','192.0.2.1',t);
 a:=app.auth_record_failure('c02a-missing@example.test','192.0.2.1',t);
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized;
 IF a.failure_count<>2 OR ip.failure_count<>2 OR a.cooldown_until<>t+interval '1 second' THEN
  RAISE EXCEPTION 'IDENTITY_GATE atomic two failures/non-sliding cooldown'; END IF;
 IF EXISTS(SELECT FROM app.users WHERE email_normalized=a.identifier_normalized) THEN
  RAISE EXCEPTION 'IDENTITY_GATE missing account fixture unexpectedly exists'; END IF;
 a:=app.auth_record_failure('c02a-missing@example.test','192.0.2.1',t+interval '2 seconds');
 IF a.cooldown_until<>t+interval '6 seconds' THEN RAISE EXCEPTION 'IDENTITY_GATE progressive backoff'; END IF;
 FOR i IN 1..9 LOOP
  a:=app.auth_record_failure('c02a-cap@example.test','192.0.2.2',t);
  IF a.cooldown_until>t+interval '60 seconds' THEN RAISE EXCEPTION 'IDENTITY_GATE backoff cap'; END IF;
  t:=a.cooldown_until+interval '1 second';
 END LOOP;
 IF a.cooldown_until<>a.last_failed_at+interval '60 seconds' THEN RAISE EXCEPTION 'IDENTITY_GATE cap reached'; END IF;
 UPDATE app.auth_account_attempts SET failure_count=1000000 WHERE identifier_normalized=a.identifier_normalized;
 a:=app.auth_record_failure(a.identifier_normalized,'192.0.2.2',t);
 IF a.failure_count<>1000000 OR a.cooldown_until<>t+interval '60 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE saturating count/backoff'; END IF;
 t:='2040-01-01Z';
 FOR i IN 1..5 LOOP PERFORM app.auth_record_failure('c02a-ip@example.test','192.0.2.3',t); END LOOP;
 PERFORM app.auth_record_failure('c02a-ip@example.test','192.0.2.3',t+interval '1 second');
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized='c02a-ip@example.test';
 IF ip.failure_count<>6 OR ip.cooldown_until<>t+interval '60 seconds' THEN
  RAISE EXCEPTION 'IDENTITY_GATE IP lockout must not slide'; END IF;
 IF ip.cooldown_until>=t+interval '61 seconds' THEN RAISE EXCEPTION 'IDENTITY_GATE cooldown expiry'; END IF;
 a:=app.auth_record_failure('c02a-ip@example.test','192.0.2.3',t+interval '16 minutes');
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized;
 IF a.failure_count<>1 OR ip.failure_count<>1 THEN RAISE EXCEPTION 'IDENTITY_GATE counter window reset'; END IF;
 -- Crossing the counting window cannot shorten an already-active cooldown.
 PERFORM app.auth_record_failure('c02a-window@example.test','192.0.2.5',t);
 FOR i IN 1..5 LOOP PERFORM app.auth_record_failure('c02a-window@example.test','192.0.2.5',t+interval '14 minutes 59 seconds'); END LOOP;
 SELECT * INTO STRICT ip FROM app.auth_login_attempts WHERE identifier_normalized='c02a-window@example.test';
 a:=app.auth_record_failure('c02a-window@example.test','192.0.2.5',t+interval '15 minutes');
 IF a.failure_count<>1 OR a.cooldown_until<>t+interval '15 minutes 1 second' OR
  (SELECT cooldown_until FROM app.auth_login_attempts WHERE identifier_normalized=a.identifier_normalized)<>ip.cooldown_until THEN
  RAISE EXCEPTION 'IDENTITY_GATE window reset changed active cooldown'; END IF;
 BEGIN PERFORM app.auth_record_failure('','192.0.2.4',t); RAISE EXCEPTION 'IDENTITY_GATE empty identifier';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.auth_record_failure('x','192.0.2.0/24',t); RAISE EXCEPTION 'IDENTITY_GATE network is not an IP';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN PERFORM app.auth_record_failure('x','192.0.2.4','infinity'); RAISE EXCEPTION 'IDENTITY_GATE infinite clock';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
END $counter$;
DO $device$
DECLARE d uuid:=current_setting('test.identity_device')::uuid; row app.known_devices; i int;
BEGIN
 FOR i IN 1..5 LOOP row:=app.auth_fail_known_device(d,'2040-01-02Z'); END LOOP;
 IF row.failure_count<>5 OR row.revoked_at IS NULL THEN RAISE EXCEPTION 'IDENTITY_GATE device five-failure revocation'; END IF;
 row:=app.auth_fail_known_device(d,'2040-01-02Z');
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
 IF EXISTS(SELECT FROM app.users WHERE email='c02a-new@example.test') OR
 EXISTS(SELECT FROM app.tenants WHERE name='new tenant') THEN RAISE EXCEPTION 'IDENTITY_GATE refused bootstrap wrote data'; END IF;
END $rollback$;
ROLLBACK;
SELECT 'PASS: global auth counters, finite non-sliding backoff, device revocation, public key uniqueness and real-role denial' AS result;
