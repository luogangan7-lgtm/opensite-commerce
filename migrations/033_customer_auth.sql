-- Responsibility: Customer mailbox proof, sessions, reset, durable admission and guest links.
-- Dependencies: customers_b2b and orders; callers: dedicated customer-auth sqlc runner.
-- Invariants: credential writes require proof; customer-first locks fence resets;
-- immutable order snapshots are read only; global/IP quotas span all tenants.
-- Rollback: retain additive schema.
DO $defaults$ DECLARE found text; BEGIN
 SELECT string_agg(DISTINCT a.privilege_type,',') INTO found FROM pg_default_acl d
 CROSS JOIN LATERAL (SELECT grantee,privilege_type FROM aclexplode(d.defaclacl)
  EXCEPT SELECT grantee,privilege_type FROM aclexplode(acldefault(d.defaclobjtype,d.defaclrole)) WHERE d.defaclnamespace=0) a
 WHERE d.defaclrole=current_user::regrole AND d.defaclnamespace IN(0,'app'::regnamespace)
 AND(a.grantee=0 OR a.grantee IN(SELECT oid FROM pg_roles WHERE starts_with(rolname,'app_')));
 IF found IS NOT NULL THEN RAISE EXCEPTION 'CUSTOMER_AUTH_DEFAULT_PRIVILEGES_UNSAFE %',found; END IF;
END $defaults$;
DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_customer_executor') THEN
  CREATE ROLE app_customer_executor NOLOGIN NOINHERIT;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_customer_executor' AND
  (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
  OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_customer_executor'::regrole OR roleid='app_customer_executor'::regrole)
  OR has_schema_privilege('app_customer_executor','app','CREATE') THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_EXECUTOR_UNSAFE';
 END IF;
 IF EXISTS(SELECT FROM pg_class WHERE relnamespace='app'::regnamespace AND relowner='app_customer_executor'::regrole)
  OR EXISTS(SELECT FROM pg_proc WHERE pronamespace='app'::regnamespace AND proowner='app_customer_executor'::regrole)
  OR EXISTS(SELECT FROM pg_class c WHERE c.relnamespace='app'::regnamespace AND c.relkind IN('r','p') AND
   (has_table_privilege('app_customer_executor',c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
    OR has_any_column_privilege('app_customer_executor',c.oid,'SELECT,INSERT,UPDATE,REFERENCES')))
  OR EXISTS(SELECT FROM pg_proc WHERE pronamespace='app'::regnamespace AND has_function_privilege('app_customer_executor',oid,'EXECUTE')) THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_EXECUTOR_RESIDUAL_CAPABILITY';
 END IF;
END $role$;
ALTER ROLE app_customer_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_customer_executor;
ALTER TABLE app.customers ADD COLUMN verified_at timestamptz CHECK(verified_at IS NULL OR isfinite(verified_at));
CREATE TABLE app.customer_auth_tokens(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), customer_id uuid NOT NULL,
 token_hash bytea NOT NULL CHECK(octet_length(token_hash)=32),
 purpose text NOT NULL CHECK(purpose IN('register','reset')),
 email_normalized text NOT NULL, pending_password_hash text, credential_version timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,token_hash),
 FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 CONSTRAINT customer_auth_token_purpose CHECK((purpose='register' AND pending_password_hash IS NOT NULL AND credential_version IS NULL)
  OR(purpose='reset' AND pending_password_hash IS NULL AND credential_version IS NOT NULL)),
 CONSTRAINT customer_auth_pending_phc CHECK(pending_password_hash IS NULL OR
  (length(pending_password_hash)<=1024 AND pending_password_hash ~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$')),
 CHECK(isfinite(expires_at) AND expires_at>created_at),
 CHECK(consumed_at IS NULL OR(isfinite(consumed_at) AND consumed_at>=created_at))
);
CREATE TABLE app.customer_sessions(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), customer_id uuid NOT NULL,
 token_hash bytea NOT NULL CHECK(octet_length(token_hash)=32), credential_version timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), expires_at timestamptz NOT NULL,
 idle_expires_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(), revoked_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,token_hash),
 FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 CHECK(isfinite(expires_at) AND isfinite(idle_expires_at) AND created_at<idle_expires_at AND idle_expires_at<=expires_at),
 CHECK(isfinite(last_seen_at) AND last_seen_at>=created_at),
 CHECK(revoked_at IS NULL OR(isfinite(revoked_at) AND revoked_at>=created_at))
);
CREATE INDEX customer_sessions_customer ON app.customer_sessions(tenant_id,customer_id);
CREATE TABLE app.customer_guest_order_links(
 tenant_id uuid NOT NULL, customer_id uuid NOT NULL, order_id uuid NOT NULL,
 linked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,order_id),
 FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id)
);
CREATE INDEX customer_guest_order_links_customer ON app.customer_guest_order_links(tenant_id,customer_id,order_id);
CREATE TABLE app.customer_auth_limits(
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), kind text NOT NULL CHECK(kind='account'),
 key_hash bytea NOT NULL CHECK(octet_length(key_hash)=32), window_start timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 11),
 failures integer NOT NULL DEFAULT 0 CHECK(failures BETWEEN 0 AND 10),
 cooldown_until timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(tenant_id,kind,key_hash)
);
-- Global authentication counters have no tenant identity or tenant policy.
-- Only the private proof executor may read/debit these opaque keys.
CREATE TABLE app.customer_auth_global_limits(
 kind text NOT NULL CHECK(kind IN('global','ip')), key_hash bytea NOT NULL CHECK(octet_length(key_hash)=32),
 window_start timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND CASE kind WHEN 'global' THEN 301 ELSE 31 END),
 failures integer NOT NULL DEFAULT 0 CHECK(failures BETWEEN 0 AND 10),
 cooldown_until timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(kind,key_hash),
 CHECK(kind<>'global' OR key_hash=decode(repeat('00',32),'hex'))
);
REVOKE ALL ON app.customer_auth_global_limits FROM PUBLIC;
DO $global_acl$ DECLARE role_name text; BEGIN
 FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  EXECUTE format('REVOKE ALL ON app.customer_auth_global_limits FROM %I',role_name);
 END LOOP;
END $global_acl$;
DO $tables$ DECLARE tab text; role_name text; BEGIN
 FOREACH tab IN ARRAY ARRAY['customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',tab);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',tab);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC',tab);
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   EXECUTE format('REVOKE ALL ON app.%I FROM %I',tab,role_name);
  END LOOP;
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_customer_auth,app_customer_executor USING(true) WITH CHECK(true)',tab);
 END LOOP;
END $tables$;
CREATE POLICY customer_executor_scope ON app.customers TO app_customer_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY credential_executor_scope ON app.customer_credentials TO app_customer_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY customer_executor_orders ON app.orders TO app_customer_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
-- An unverified guest row is harmless; retain the existing customer INSERT port.
-- Creating/changing a credential is now exclusively a proof-bearing command.
REVOKE INSERT(tenant_id,customer_id,password_hash),UPDATE(password_hash) ON app.customer_credentials FROM app_customer_auth;
GRANT SELECT ON app.customer_auth_tokens,app.customer_sessions,app.customer_guest_order_links TO app_customer_auth;
GRANT SELECT ON app.customers,app.customer_credentials,app.customer_auth_tokens,app.customer_sessions,
 app.customer_guest_order_links,app.customer_auth_limits,app.customer_auth_global_limits TO app_customer_executor;
GRANT SELECT(id,status) ON app.tenants TO app_customer_executor;
GRANT SELECT(tenant_id,id,customer_id,guest_contact,sealed) ON app.orders TO app_customer_executor;
GRANT INSERT(tenant_id,id,email,display_name) ON app.customers TO app_customer_executor;
GRANT UPDATE(verified_at) ON app.customers TO app_customer_executor;
GRANT INSERT(tenant_id,customer_id,password_hash) ON app.customer_credentials TO app_customer_executor;
GRANT UPDATE(password_hash) ON app.customer_credentials TO app_customer_executor;
GRANT INSERT(tenant_id,customer_id,token_hash,purpose,email_normalized,pending_password_hash,credential_version,expires_at) ON app.customer_auth_tokens TO app_customer_executor;
GRANT UPDATE(consumed_at) ON app.customer_auth_tokens TO app_customer_executor;
GRANT INSERT(tenant_id,customer_id,token_hash,credential_version,expires_at,idle_expires_at) ON app.customer_sessions TO app_customer_executor;
GRANT UPDATE(revoked_at,idle_expires_at,last_seen_at) ON app.customer_sessions TO app_customer_executor;
GRANT INSERT(tenant_id,customer_id,order_id) ON app.customer_guest_order_links TO app_customer_executor;
GRANT INSERT(tenant_id,kind,key_hash) ON app.customer_auth_limits TO app_customer_executor;
GRANT UPDATE(window_start,attempts,failures,cooldown_until) ON app.customer_auth_limits TO app_customer_executor;
GRANT INSERT(kind,key_hash) ON app.customer_auth_global_limits TO app_customer_executor;
GRANT UPDATE(window_start,attempts,failures,cooldown_until) ON app.customer_auth_global_limits TO app_customer_executor;

CREATE FUNCTION app.customer_auth_context() RETURNS uuid LANGUAGE sql STABLE SECURITY INVOKER
 SET search_path=pg_catalog AS $fn$
 SELECT id FROM app.tenants WHERE id=nullif(current_setting('app.tenant_id',true),'')::uuid AND status='active'
$fn$;
CREATE FUNCTION app.customer_claim_guest_orders(p_customer uuid) RETURNS bigint
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); email text; n bigint; BEGIN
 SELECT c.email_normalized INTO email FROM app.customers c WHERE c.tenant_id=t AND c.id=p_customer
  AND c.status='active' AND c.verified_at IS NOT NULL FOR UPDATE;
 IF NOT FOUND THEN RETURN 0; END IF;
 INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id)
 SELECT t,p_customer,o.id FROM app.orders o WHERE o.tenant_id=t AND o.sealed AND o.customer_id IS NULL
  AND lower(regexp_replace(o.guest_contact->>'email','^[[:space:]]+|[[:space:]]+$','','g'))=email
 ON CONFLICT DO NOTHING;
 GET DIAGNOSTICS n=ROW_COUNT; RETURN n;
END $fn$;
CREATE FUNCTION app.customer_prepare_registration(p_email text,p_hash text,p_token bytea,p_expires timestamptz) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); c app.customers%ROWTYPE; BEGIN
 IF t IS NULL THEN RETURN false; END IF;
 INSERT INTO app.customers(tenant_id,email) VALUES(t,p_email) ON CONFLICT(tenant_id,email_normalized) DO NOTHING;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t
  AND email_normalized=lower(regexp_replace(p_email,'^[[:space:]]+|[[:space:]]+$','','g')) FOR UPDATE;
 IF c.status<>'active' OR(c.verified_at IS NOT NULL AND EXISTS(SELECT FROM app.customer_credentials WHERE tenant_id=t AND customer_id=c.id)) THEN RETURN false; END IF;
 INSERT INTO app.customer_auth_tokens(tenant_id,customer_id,token_hash,purpose,email_normalized,pending_password_hash,expires_at)
 VALUES(t,c.id,p_token,'register',c.email_normalized,p_hash,p_expires);
 RETURN true;
END $fn$;
CREATE FUNCTION app.customer_consume_verification(p_token bytea) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); cid uuid; c app.customers%ROWTYPE; candidate text; BEGIN
 SELECT customer_id INTO cid FROM app.customer_auth_tokens WHERE tenant_id=t AND token_hash=p_token
  AND purpose='register' AND consumed_at IS NULL AND expires_at>clock_timestamp();
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=cid FOR UPDATE;
 IF c.status<>'active' OR(c.verified_at IS NOT NULL AND EXISTS(SELECT FROM app.customer_credentials WHERE tenant_id=t AND customer_id=cid)) THEN RETURN false; END IF;
 UPDATE app.customer_auth_tokens SET consumed_at=clock_timestamp() WHERE tenant_id=t AND token_hash=p_token
  AND purpose='register' AND consumed_at IS NULL AND expires_at>clock_timestamp() AND email_normalized=c.email_normalized
 RETURNING pending_password_hash INTO candidate;
 IF NOT FOUND THEN RETURN false; END IF;
 -- Legacy unverified credentials survive mailbox proof unchanged. A subsequent
 -- password reset is the explicit way to change them; proof never replaces them.
 INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash) VALUES(t,cid,candidate)
 ON CONFLICT DO NOTHING;
 UPDATE app.customers SET verified_at=clock_timestamp() WHERE tenant_id=t AND id=cid;
 PERFORM app.customer_claim_guest_orders(cid); RETURN true;
END $fn$;
CREATE FUNCTION app.customer_prepare_reset(p_email text,p_token bytea,p_expires timestamptz) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); c app.customers%ROWTYPE; stamp timestamptz; BEGIN
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND status='active' AND verified_at IS NOT NULL
  AND email_normalized=lower(regexp_replace(p_email,'^[[:space:]]+|[[:space:]]+$','','g')) FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT updated_at INTO stamp FROM app.customer_credentials WHERE tenant_id=t AND customer_id=c.id FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 INSERT INTO app.customer_auth_tokens(tenant_id,customer_id,token_hash,purpose,email_normalized,credential_version,expires_at)
 VALUES(t,c.id,p_token,'reset',c.email_normalized,stamp,p_expires); RETURN true;
END $fn$;
CREATE FUNCTION app.customer_consume_reset(p_token bytea,p_hash text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); cid uuid; c app.customers%ROWTYPE; stamp timestamptz; BEGIN
 IF p_hash IS NULL OR p_hash !~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$' THEN
  RAISE EXCEPTION 'CUSTOMER_AUTH_INVALID_PHC' USING ERRCODE='22023'; END IF;
 SELECT customer_id INTO cid FROM app.customer_auth_tokens WHERE tenant_id=t AND token_hash=p_token
  AND purpose='reset' AND consumed_at IS NULL AND expires_at>clock_timestamp();
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=cid FOR UPDATE;
 IF c.status<>'active' OR c.verified_at IS NULL THEN RETURN false; END IF;
 SELECT updated_at INTO stamp FROM app.customer_credentials WHERE tenant_id=t AND customer_id=cid FOR UPDATE;
 UPDATE app.customer_auth_tokens SET consumed_at=clock_timestamp() WHERE tenant_id=t AND token_hash=p_token
  AND purpose='reset' AND consumed_at IS NULL AND expires_at>clock_timestamp()
  AND email_normalized=c.email_normalized AND credential_version=stamp;
 IF NOT FOUND THEN RETURN false; END IF;
 UPDATE app.customer_credentials SET password_hash=p_hash WHERE tenant_id=t AND customer_id=cid;
 UPDATE app.customer_sessions SET revoked_at=clock_timestamp() WHERE tenant_id=t AND customer_id=cid AND revoked_at IS NULL;
 RETURN true;
END $fn$;
CREATE FUNCTION app.customer_save_session(p_customer uuid,p_expected_hash text,p_old bytea,p_token bytea,p_absolute timestamptz,p_idle timestamptz) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); c app.customers%ROWTYPE; cred app.customer_credentials%ROWTYPE; s app.customer_sessions%ROWTYPE; BEGIN
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=p_customer FOR UPDATE;
 IF NOT FOUND OR c.status<>'active' OR c.verified_at IS NULL THEN RETURN NULL; END IF;
 SELECT * INTO cred FROM app.customer_credentials WHERE tenant_id=t AND customer_id=p_customer FOR UPDATE;
 IF NOT FOUND OR cred.password_hash IS DISTINCT FROM p_expected_hash THEN RETURN NULL; END IF;
 IF p_old IS NOT NULL THEN
  UPDATE app.customer_sessions SET revoked_at=clock_timestamp() WHERE tenant_id=t AND customer_id=p_customer
   AND token_hash=p_old AND revoked_at IS NULL AND expires_at>clock_timestamp() AND idle_expires_at>clock_timestamp()
   AND credential_version=cred.updated_at;
  IF NOT FOUND THEN RETURN NULL; END IF;
 END IF;
 INSERT INTO app.customer_sessions(tenant_id,customer_id,token_hash,credential_version,expires_at,idle_expires_at)
 VALUES(t,p_customer,p_token,cred.updated_at,p_absolute,p_idle) RETURNING * INTO s;
 RETURN jsonb_build_object('session_id',s.id,'tenant_id',t,'customer_id',c.id,'email',c.email,
  'expires_at',s.expires_at,'idle_expires_at',s.idle_expires_at);
END $fn$;
CREATE FUNCTION app.customer_session(p_token bytea,p_idle_seconds bigint) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); cid uuid; c app.customers%ROWTYPE; stamp timestamptz; s app.customer_sessions%ROWTYPE; BEGIN
 IF p_idle_seconds NOT BETWEEN 1 AND 86400 THEN RAISE EXCEPTION 'CUSTOMER_AUTH_INVALID_IDLE' USING ERRCODE='22023'; END IF;
 SELECT customer_id INTO cid FROM app.customer_sessions WHERE token_hash=p_token;
 IF NOT FOUND THEN RETURN NULL; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=cid FOR UPDATE;
 IF c.status<>'active' OR c.verified_at IS NULL THEN RETURN NULL; END IF;
 SELECT updated_at INTO stamp FROM app.customer_credentials WHERE tenant_id=t AND customer_id=cid FOR UPDATE;
 UPDATE app.customer_sessions SET last_seen_at=clock_timestamp(),
  idle_expires_at=least(expires_at,clock_timestamp()+make_interval(secs=>p_idle_seconds::double precision))
 WHERE tenant_id=t AND token_hash=p_token AND revoked_at IS NULL AND credential_version=stamp
  AND expires_at>clock_timestamp() AND idle_expires_at>clock_timestamp() RETURNING * INTO s;
 IF NOT FOUND THEN RETURN NULL; END IF;
 RETURN jsonb_build_object('session_id',s.id,'tenant_id',t,'customer_id',c.id,'email',c.email,
  'expires_at',s.expires_at,'idle_expires_at',s.idle_expires_at);
END $fn$;
CREATE FUNCTION app.customer_revoke_session(p_token bytea) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); cid uuid; BEGIN
 SELECT customer_id INTO cid FROM app.customer_sessions WHERE token_hash=p_token;
 IF NOT FOUND THEN RETURN false; END IF;
 PERFORM 1 FROM app.customers WHERE tenant_id=t AND id=cid FOR UPDATE;
 UPDATE app.customer_sessions SET revoked_at=clock_timestamp() WHERE tenant_id=t AND token_hash=p_token AND revoked_at IS NULL;
 RETURN FOUND;
END $fn$;
CREATE FUNCTION app.customer_admit(p_account bytea,p_ip bytea,p_now timestamptz) RETURNS bigint
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); kind_name text; k bytea; cap integer;
 shared app.customer_auth_global_limits%ROWTYPE; account app.customer_auth_limits%ROWTYPE;
 n timestamptz; delay bigint:=0; BEGIN
 IF t IS NULL OR octet_length(p_account)<>32 OR octet_length(p_ip)<>32 OR p_account IS NULL OR p_ip IS NULL THEN RETURN 60; END IF;
 -- ponytail: one global row bounds admitted key allocation across all tenants; shard only after measured contention.
 -- Lock/debit global -> IP -> tenant account. A refusal never allocates a downstream key.
 FOREACH kind_name IN ARRAY ARRAY['global','ip'] LOOP
  k:=CASE kind_name WHEN 'global' THEN decode(repeat('00',32),'hex') ELSE p_ip END;
  cap:=CASE kind_name WHEN 'global' THEN 300 ELSE 30 END;
  INSERT INTO app.customer_auth_global_limits(kind,key_hash) VALUES(kind_name,k) ON CONFLICT DO NOTHING;
  SELECT * INTO shared FROM app.customer_auth_global_limits WHERE kind=kind_name AND key_hash=k FOR UPDATE;
  -- p_now is signature-compatible only; the server owns windows even after a lock wait.
  n:=clock_timestamp();
  IF shared.window_start+interval '1 minute'<=n THEN shared.window_start:=n; shared.attempts:=0; END IF;
  shared.attempts:=least(cap+1,shared.attempts+1);
  UPDATE app.customer_auth_global_limits SET window_start=shared.window_start,attempts=shared.attempts WHERE kind=kind_name AND key_hash=k;
  IF shared.attempts>cap THEN delay:=greatest(delay,ceil(extract(epoch FROM shared.window_start+interval '1 minute'-n))::bigint); END IF;
  IF shared.cooldown_until>n THEN delay:=greatest(delay,ceil(extract(epoch FROM shared.cooldown_until-n))::bigint); END IF;
  IF delay>0 THEN RETURN delay; END IF;
 END LOOP;
 INSERT INTO app.customer_auth_limits(tenant_id,kind,key_hash) VALUES(t,'account',p_account) ON CONFLICT DO NOTHING;
 SELECT * INTO account FROM app.customer_auth_limits WHERE tenant_id=t AND kind='account' AND key_hash=p_account FOR UPDATE;
 n:=clock_timestamp();
 IF account.window_start+interval '1 minute'<=n THEN account.window_start:=n; account.attempts:=0; END IF;
 account.attempts:=least(11,account.attempts+1);
 UPDATE app.customer_auth_limits SET window_start=account.window_start,attempts=account.attempts WHERE tenant_id=t AND kind='account' AND key_hash=p_account;
 IF account.attempts>10 THEN delay:=greatest(delay,ceil(extract(epoch FROM account.window_start+interval '1 minute'-n))::bigint); END IF;
 IF account.cooldown_until>n THEN delay:=greatest(delay,ceil(extract(epoch FROM account.cooldown_until-n))::bigint); END IF;
 RETURN delay;
END $fn$;
CREATE FUNCTION app.customer_failed(p_account bytea,p_ip bytea) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $fn$
DECLARE t uuid:=app.customer_auth_context(); n timestamptz;
 shared app.customer_auth_global_limits%ROWTYPE; account app.customer_auth_limits%ROWTYPE; BEGIN
 IF t IS NULL OR octet_length(p_account)<>32 OR octet_length(p_ip)<>32 OR p_account IS NULL OR p_ip IS NULL THEN RETURN false; END IF;
 -- Same global -> IP -> account order as admission; missing admission allocates no rows.
 PERFORM 1 FROM app.customer_auth_global_limits WHERE kind='global' AND key_hash=decode(repeat('00',32),'hex') FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT * INTO shared FROM app.customer_auth_global_limits WHERE kind='ip' AND key_hash=p_ip FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 n:=clock_timestamp(); shared.failures:=least(10,shared.failures+1);
 UPDATE app.customer_auth_global_limits SET failures=shared.failures,
  cooldown_until=CASE WHEN shared.cooldown_until>n OR shared.failures<5 THEN shared.cooldown_until
   ELSE n+make_interval(secs=>least(60,5*(1<<(shared.failures-5)))) END
 WHERE kind='ip' AND key_hash=p_ip;
 SELECT * INTO account FROM app.customer_auth_limits WHERE tenant_id=t AND kind='account' AND key_hash=p_account FOR UPDATE;
 IF FOUND THEN
  n:=clock_timestamp(); account.failures:=least(10,account.failures+1);
  UPDATE app.customer_auth_limits SET failures=account.failures,
   cooldown_until=CASE WHEN account.cooldown_until>n OR account.failures<5 THEN account.cooldown_until
    ELSE n+make_interval(secs=>least(60,5*(1<<(account.failures-5)))) END
  WHERE tenant_id=t AND kind='account' AND key_hash=p_account;
 END IF;
 RETURN true;
END $fn$;
GRANT CREATE ON SCHEMA app TO app_customer_executor;
DO $functions$ DECLARE f record; role_name text; BEGIN
 FOR f IN SELECT p.oid,p.proname FROM pg_proc p WHERE p.pronamespace='app'::regnamespace
  AND p.proname=ANY(ARRAY['customer_auth_context','customer_claim_guest_orders','customer_prepare_registration',
   'customer_consume_verification','customer_prepare_reset','customer_consume_reset','customer_save_session',
   'customer_session','customer_revoke_session','customer_admit','customer_failed']) LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_customer_executor',f.oid::regprocedure);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.oid::regprocedure);
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_customer_executor' LOOP
   EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.oid::regprocedure,role_name);
  END LOOP;
  IF f.proname<>'customer_auth_context' THEN
   EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO app_customer_auth',f.oid::regprocedure);
  END IF;
 END LOOP;
END $functions$;
REVOKE CREATE ON SCHEMA app FROM app_customer_executor;
