-- stable-id: h4-identity
-- depends-on: h4-clock
-- Responsibility: serialize credential login with every password writer.
-- Dependencies: global identity users/sessions; called by private login store.
-- Invariants: user before session locks, post-wait PHC fence, atomic revoke-all.
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
