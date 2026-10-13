-- Responsibility: password/session fence and least-privilege witnesses for H-4.
-- Dependencies: all forward migrations, app_auth/credential writer roles.
-- Called by CI PG and upgrade gates; same-value updates preserve valid sessions.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.users(email,password_hash)
VALUES('h4-reset@example.test',encode(sha256(uuidv7()::text::bytea),'hex')) RETURNING id AS uid \gset
SELECT set_config('test.h4_user',:'uid',true);
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at)
SELECT :'uid',sha256(uuidv7()::text::bytea),clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '1 hour'
FROM generate_series(1,2);
SET LOCAL ROLE app_credential_writer;
UPDATE app.users SET password_hash=password_hash WHERE id=current_setting('test.h4_user')::uuid;
RESET ROLE;
DO $same$
BEGIN
 IF (SELECT count(*) FROM app.auth_sessions WHERE user_id=current_setting('test.h4_user')::uuid AND revoked_at IS NULL)<>2
 THEN RAISE EXCEPTION 'H4_PASSWORD_GATE same-value update revoked sessions'; END IF;
END $same$;
SET LOCAL ROLE app_credential_writer;
UPDATE app.users
SET password_hash=encode(sha256(uuidv7()::text::bytea),'hex')
WHERE id=current_setting('test.h4_user')::uuid;
RESET ROLE;
DO $changed$
BEGIN
 IF EXISTS(SELECT FROM app.auth_sessions WHERE user_id=current_setting('test.h4_user')::uuid AND revoked_at IS NULL)
 THEN RAISE EXCEPTION 'H4_PASSWORD_GATE password change left active session'; END IF;
END $changed$;
SET LOCAL ROLE app_auth;
DO $auth$
BEGIN
 BEGIN
  UPDATE app.users
  SET password_hash=encode(sha256(uuidv7()::text::bytea),'hex')
  WHERE id=current_setting('test.h4_user')::uuid;
  RAISE EXCEPTION 'H4_PASSWORD_GATE auth mutated password';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $auth$;
RESET ROLE;
ROLLBACK;
\echo 'H4_PASSWORD_GATE=PASS'
