-- Responsibility: global staff-auth bookkeeping and offline first-owner capability.
-- Dependencies: all preceding migrations; follows 001 global users/sessions.
-- Callers: trusted app_auth pool and execute-only offline bootstrap LOGIN.
-- Invariants: no tenant custody realm; exact column ACLs; bounded counters;
-- account then IP lock order; irreversible device revocation; one first owner.
CREATE TABLE app.auth_account_attempts (
 identifier_normalized text PRIMARY KEY CHECK (octet_length(identifier_normalized) BETWEEN 1 AND 320 AND identifier_normalized=lower(btrim(identifier_normalized))),
 failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 1000000),
 window_started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 cooldown_until timestamptz NOT NULL DEFAULT '-infinity',
 last_failed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE app.auth_login_attempts (
 identifier_normalized text NOT NULL REFERENCES app.auth_account_attempts(identifier_normalized),
 ip inet NOT NULL CHECK (masklen(ip)=CASE family(ip) WHEN 4 THEN 32 ELSE 128 END),
 failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 1000000),
 window_started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 cooldown_until timestamptz NOT NULL DEFAULT '-infinity',
 last_failed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (identifier_normalized,ip)
);
CREATE TABLE app.known_devices (
 id uuid PRIMARY KEY DEFAULT uuidv7(),
 user_id uuid NOT NULL REFERENCES app.users(id),
 cookie_hash bytea NOT NULL UNIQUE CHECK (octet_length(cookie_hash)=32),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 5),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (isfinite(expires_at) AND expires_at>created_at),
 CHECK (failure_count<5 OR revoked_at IS NOT NULL)
);
-- Unlike business data these three tables are GLOBAL, with no tenant_id/RLS.
-- Default privileges and inherited PUBLIC grants must never make them public.
DO $global_acl$
DECLARE t text; cols text; r record;
BEGIN
 FOREACH t IN ARRAY ARRAY['auth_account_attempts','auth_login_attempts','known_devices'] LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO cols FROM pg_attribute
   WHERE attrelid=format('app.%I',t)::regclass AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC',t);
  EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM PUBLIC',cols,cols,cols,cols,t);
  FOR r IN SELECT rolname FROM pg_roles WHERE rolname LIKE 'app\_%' ESCAPE '\' OR oid IN (
   SELECT acl.grantee FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) acl
    WHERE c.oid=format('app.%I',t)::regclass AND acl.grantee<>c.relowner
   UNION SELECT acl.grantee FROM pg_attribute a CROSS JOIN LATERAL aclexplode(a.attacl) acl
    WHERE a.attrelid=format('app.%I',t)::regclass) LOOP
   EXECUTE format('REVOKE ALL ON app.%I FROM %I',t,r.rolname);
   EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM %I',cols,cols,cols,cols,t,r.rolname);
  END LOOP;
 END LOOP;
END $global_acl$;
GRANT SELECT(identifier_normalized,failure_count,window_started_at,cooldown_until,last_failed_at),
 INSERT(identifier_normalized,failure_count,window_started_at,cooldown_until,last_failed_at),
 UPDATE(failure_count,window_started_at,cooldown_until,last_failed_at) ON app.auth_account_attempts TO app_auth;
GRANT SELECT(identifier_normalized,ip,failure_count,window_started_at,cooldown_until,last_failed_at),
 INSERT(identifier_normalized,ip,failure_count,window_started_at,cooldown_until,last_failed_at),
 UPDATE(failure_count,window_started_at,cooldown_until,last_failed_at) ON app.auth_login_attempts TO app_auth;
GRANT SELECT(id,user_id,cookie_hash,expires_at,revoked_at,failure_count,created_at),
 INSERT(id,user_id,cookie_hash,expires_at,created_at), UPDATE(revoked_at,failure_count) ON app.known_devices TO app_auth;
GRANT SELECT(id,status) ON app.tenants TO app_auth;
CREATE UNIQUE INDEX api_keys_public_id_unique ON app.api_keys(id);

DO $roles$
DECLARE n text; r record; t record; cols text;
BEGIN
 FOREACH n IN ARRAY ARRAY['app_credential_writer','app_admin_bootstrap'] LOOP
  IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname=n) THEN EXECUTE format('CREATE ROLE %I NOLOGIN',n); END IF;
  EXECUTE format('ALTER ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT',n);
  FOR r IN SELECT DISTINCT p.rolname FROM pg_auth_members m JOIN pg_roles p ON p.oid=m.roleid WHERE m.member=n::regrole LOOP
   EXECUTE format('REVOKE %I FROM %I CASCADE',r.rolname,n);
  END LOOP;
  EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA app FROM %I',n);
  EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM %I',n);
  EXECUTE format('REVOKE ALL ON SCHEMA app FROM %I',n);
  FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace s ON s.oid=c.relnamespace WHERE s.nspname='app' AND c.relkind IN ('r','p') LOOP
   SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO cols FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
   EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM %I',cols,cols,cols,cols,t.relname,n);
  END LOOP;
 END LOOP;
 FOR r IN SELECT u.rolname FROM pg_auth_members m JOIN pg_roles u ON u.oid=m.member WHERE m.roleid='app_credential_writer'::regrole LOOP
  EXECUTE format('REVOKE app_credential_writer FROM %I CASCADE',r.rolname);
 END LOOP;
END $roles$;
GRANT USAGE ON SCHEMA app TO app_credential_writer,app_admin_bootstrap;
GRANT INSERT(email,password_hash),SELECT(id),UPDATE(password_hash) ON app.users TO app_credential_writer;
GRANT INSERT(name),SELECT(id) ON app.tenants TO app_credential_writer;
GRANT INSERT(tenant_id,user_id,role),SELECT(role) ON app.tenant_memberships TO app_credential_writer;
GRANT INSERT(tenant_id,id,issued_by,key_prefix,key_hash,scopes,expires_at),SELECT(id) ON app.api_keys TO app_credential_writer;
-- This global policy is READ ONLY, to observe an owner in any business tenant.
CREATE POLICY credential_owner_read ON app.tenant_memberships FOR SELECT TO app_credential_writer USING (true);
CREATE POLICY credential_membership_insert ON app.tenant_memberships FOR INSERT TO app_credential_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY credential_key_scope ON app.api_keys TO app_credential_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

CREATE FUNCTION app.auth_record_failure(p_identifier text,p_ip inet,p_now timestamptz DEFAULT NULL)
RETURNS app.auth_account_attempts LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $failure$
DECLARE k text:=lower(btrim(p_identifier)); t timestamptz; a app.auth_account_attempts; i app.auth_login_attempts;
BEGIN
 IF k IS NULL OR octet_length(k) NOT BETWEEN 1 AND 320 OR p_ip IS NULL OR
  masklen(p_ip)<>(CASE family(p_ip) WHEN 4 THEN 32 ELSE 128 END) OR (p_now IS NOT NULL AND NOT isfinite(p_now)) THEN
  RAISE EXCEPTION 'AUTH_FAILURE_INVALID_INPUT' USING ERRCODE='22023'; END IF;
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'AUTH_REQUIRES_READ_COMMITTED' USING ERRCODE='25000'; END IF;
 -- Serialize even the first INSERT before sampling the production clock. Hash
 -- collisions only serialize unrelated identifiers. Test clock is trusted-only.
 PERFORM pg_advisory_xact_lock(hashtextextended('opensite:auth:'||k,0));
 t:=coalesce(p_now,clock_timestamp());
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

CREATE FUNCTION app.guard_known_device() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $guard$
BEGIN
 IF (NEW.id,NEW.user_id,NEW.cookie_hash,NEW.expires_at,NEW.created_at) IS DISTINCT FROM
    (OLD.id,OLD.user_id,OLD.cookie_hash,OLD.expires_at,OLD.created_at) OR NEW.failure_count<OLD.failure_count
    OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
  RAISE EXCEPTION 'KNOWN_DEVICE_IMMUTABLE_OR_REVOKED' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER known_device_guard BEFORE UPDATE ON app.known_devices FOR EACH ROW EXECUTE FUNCTION app.guard_known_device();
CREATE FUNCTION app.auth_fail_known_device(p_id uuid,p_now timestamptz DEFAULT NULL)
RETURNS app.known_devices LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $device$
DECLARE d app.known_devices; t timestamptz;
BEGIN
 IF p_now IS NOT NULL AND NOT isfinite(p_now) THEN RAISE EXCEPTION 'DEVICE_INVALID_CLOCK' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM app.known_devices WHERE id=p_id FOR UPDATE;
 t:=coalesce(p_now,clock_timestamp());
 IF NOT FOUND OR d.revoked_at IS NOT NULL OR d.expires_at<=t THEN RETURN NULL; END IF;
 UPDATE app.known_devices SET failure_count=least(5,failure_count+1),
  revoked_at=CASE WHEN failure_count>=4 THEN t ELSE revoked_at END WHERE id=p_id RETURNING * INTO d;
 RETURN d;
END $device$;

CREATE FUNCTION app.bootstrap_admin(p_email text,p_tenant_name text,p_password_hash text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $bootstrap$
DECLARE u uuid; t uuid;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'BOOTSTRAP_REQUIRES_READ_COMMITTED' USING ERRCODE='25000'; END IF;
 IF p_email IS NULL OR btrim(p_email)='' OR octet_length(p_email)>254 OR
  p_tenant_name IS NULL OR btrim(p_tenant_name)='' OR char_length(p_tenant_name)>200 OR
  p_password_hash IS NULL OR p_password_hash !~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$' THEN
  RAISE EXCEPTION 'BOOTSTRAP_INVALID_INPUT' USING ERRCODE='22023'; END IF;
 -- Installation-wide two-int namespace, disjoint from the auth int8 lock space.
 -- READ COMMITTED is required: the query after waiting must see the winner.
 PERFORM pg_advisory_xact_lock(1886417000,1);
 IF EXISTS(SELECT FROM app.tenant_memberships WHERE role='owner') THEN
  RAISE EXCEPTION 'BOOTSTRAP_OWNER_EXISTS' USING ERRCODE='55000'; END IF;
 INSERT INTO app.users(email,password_hash) VALUES(btrim(p_email),p_password_hash) RETURNING id INTO u;
 INSERT INTO app.tenants(name) VALUES(btrim(p_tenant_name)) RETURNING id INTO t;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'owner');
 RETURN t;
END $bootstrap$;
-- Ownership changes require temporary schema CREATE; revoke before commit.
GRANT CREATE ON SCHEMA app TO app_credential_writer;
ALTER FUNCTION app.auth_record_failure(text,inet,timestamptz) OWNER TO app_credential_writer;
ALTER FUNCTION app.guard_known_device() OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_fail_known_device(uuid,timestamptz) OWNER TO app_credential_writer;
ALTER FUNCTION app.bootstrap_admin(text,text,text) OWNER TO app_credential_writer;
REVOKE CREATE ON SCHEMA app FROM app_credential_writer;
REVOKE ALL ON FUNCTION app.auth_record_failure(text,inet,timestamptz),app.guard_known_device(),
 app.auth_fail_known_device(uuid,timestamptz),app.bootstrap_admin(text,text,text) FROM PUBLIC;
-- Remove custom ALTER DEFAULT PRIVILEGES grants too, not only known app roles.
DO $function_acl$
DECLARE f record; r record;
BEGIN
 FOR f IN SELECT p.oid,p.proowner,p.proacl FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname IN ('auth_record_failure','guard_known_device','auth_fail_known_device','bootstrap_admin') LOOP
  FOR r IN SELECT DISTINCT u.rolname FROM aclexplode(f.proacl) acl JOIN pg_roles u ON u.oid=acl.grantee WHERE acl.grantee<>f.proowner LOOP
   EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.oid::regprocedure,r.rolname);
  END LOOP;
 END LOOP;
END $function_acl$;
GRANT EXECUTE ON FUNCTION app.auth_record_failure(text,inet,timestamptz),app.auth_fail_known_device(uuid,timestamptz) TO app_auth;
GRANT EXECUTE ON FUNCTION app.bootstrap_admin(text,text,text) TO app_admin_bootstrap;
