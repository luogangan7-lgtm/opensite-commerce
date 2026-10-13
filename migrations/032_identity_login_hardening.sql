-- Responsibility: fence login against password changes and use post-lock DB time.
-- Dependencies: identity migrations 028/029; callers: private staff login store.
-- Invariants: user lock precedes session locks; auth never gains password writes;
-- credential writer remains unreachable; password changes revoke every session.
CREATE OR REPLACE FUNCTION app.auth_record_failure(p_identifier text,p_ip inet)
RETURNS app.auth_account_attempts LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $failure$
DECLARE k text:=lower(btrim(p_identifier)); t timestamptz; a app.auth_account_attempts; i app.auth_login_attempts;
BEGIN
 IF k IS NULL OR octet_length(k) NOT BETWEEN 1 AND 320 OR p_ip IS NULL OR
  masklen(p_ip)<>(CASE family(p_ip) WHEN 4 THEN 32 ELSE 128 END) THEN
  RAISE EXCEPTION 'AUTH_FAILURE_INVALID_INPUT' USING ERRCODE='22023'; END IF;
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'AUTH_REQUIRES_READ_COMMITTED' USING ERRCODE='25000'; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('opensite:auth:'||k,0));
 -- A transaction-start timestamp would consume the backoff while queued.
 t:=clock_timestamp();
 INSERT INTO app.auth_account_attempts AS old(identifier_normalized,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES(k,1,t,t+interval '1 second',t)
 ON CONFLICT(identifier_normalized) DO UPDATE SET
  failure_count=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 1 ELSE least(1000000,old.failure_count+1) END,
  window_started_at=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN t ELSE old.window_started_at END,
  cooldown_until=CASE WHEN old.cooldown_until>t THEN old.cooldown_until ELSE t+
   least(60,1 << least(6,CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 0 ELSE old.failure_count END))*interval '1 second' END,
  last_failed_at=greatest(old.last_failed_at,t)
 RETURNING * INTO a;
 INSERT INTO app.auth_login_attempts AS old(identifier_normalized,ip,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES(k,p_ip,1,t,t,t)
 ON CONFLICT(identifier_normalized,ip) DO UPDATE SET
  failure_count=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 1 ELSE least(1000000,old.failure_count+1) END,
  window_started_at=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN t ELSE old.window_started_at END,
  cooldown_until=CASE WHEN old.cooldown_until>t THEN old.cooldown_until
   WHEN t>=old.window_started_at+interval '15 minutes' OR old.failure_count<4 THEN t ELSE t+interval '60 seconds' END,
  last_failed_at=greatest(old.last_failed_at,t)
 RETURNING * INTO i;
 RETURN a;
END $failure$;

CREATE OR REPLACE FUNCTION app.auth_fail_known_device(p_id uuid)
RETURNS app.known_devices LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $device$
DECLARE d app.known_devices; t timestamptz;
BEGIN
 SELECT * INTO d FROM app.known_devices WHERE id=p_id FOR UPDATE;
 t:=clock_timestamp();
 IF NOT FOUND OR d.revoked_at IS NOT NULL OR d.expires_at<=t THEN RETURN NULL; END IF;
 UPDATE app.known_devices SET failure_count=least(5,failure_count+1),
  revoked_at=CASE WHEN failure_count>=4 THEN t ELSE revoked_at END WHERE id=p_id RETURNING * INTO d;
 RETURN d;
END $device$;

-- Row locking requires UPDATE authority. The definer grants only this lock and
-- a boolean comparison, keeping app_auth unable to mutate identity credentials.
GRANT SELECT(password_hash,status) ON app.users TO app_credential_writer;
GRANT SELECT(user_id,revoked_at),UPDATE(revoked_at) ON app.auth_sessions TO app_credential_writer;
CREATE FUNCTION app.auth_lock_login_user(p_user_id uuid,p_verified_password_hash text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $user_lock$
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'AUTH_REQUIRES_READ_COMMITTED' USING ERRCODE='25000'; END IF;
 IF p_user_id IS NULL OR p_verified_password_hash IS NULL OR p_verified_password_hash='' THEN RETURN false; END IF;
 PERFORM id FROM app.users WHERE id=p_user_id FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 RETURN EXISTS(SELECT FROM app.users WHERE id=p_user_id AND status='active' AND password_hash=p_verified_password_hash);
END $user_lock$;

CREATE FUNCTION app.auth_revoke_user_sessions() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $revoke$
BEGIN
 IF NEW.password_hash IS DISTINCT FROM OLD.password_hash THEN
  -- UPDATE already owns the user lock, matching the login's lock order.
  UPDATE app.auth_sessions SET revoked_at=clock_timestamp()
   WHERE user_id=NEW.id AND revoked_at IS NULL;
 END IF;
 RETURN NEW;
END $revoke$;
CREATE TRIGGER users_password_session_revoke AFTER UPDATE OF password_hash ON app.users
 FOR EACH ROW EXECUTE FUNCTION app.auth_revoke_user_sessions();

GRANT CREATE ON SCHEMA app TO app_credential_writer;
ALTER FUNCTION app.auth_lock_login_user(uuid,text) OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_revoke_user_sessions() OWNER TO app_credential_writer;
REVOKE CREATE ON SCHEMA app FROM app_credential_writer;
REVOKE ALL ON FUNCTION app.auth_lock_login_user(uuid,text),app.auth_revoke_user_sessions() FROM PUBLIC;
-- Default ACLs can name arbitrary custom roles; strip them before the exact grant.
DO $function_acl$
DECLARE f record; r record;
BEGIN
 FOR f IN SELECT p.oid,p.proowner,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname IN ('auth_lock_login_user','auth_revoke_user_sessions') LOOP
  FOR r IN SELECT DISTINCT u.rolname FROM aclexplode(f.proacl) acl JOIN pg_roles u ON u.oid=acl.grantee WHERE acl.grantee<>f.proowner LOOP
   EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.oid::regprocedure,r.rolname);
  END LOOP;
 END LOOP;
END $function_acl$;
GRANT EXECUTE ON FUNCTION app.auth_lock_login_user(uuid,text) TO app_auth;
