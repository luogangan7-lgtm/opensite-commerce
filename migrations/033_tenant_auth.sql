-- Responsibility: tenant-bound API-key custody and transaction-time authority.
-- Dependencies: identity users/sessions and tenant memberships; app_auth calls
-- key capabilities, app_runtime calls the boolean write-authority capability.
-- Invariants: no runtime credential reads or auth key DML; locks precede fresh
-- database-clock decisions and remain held through the caller's transaction.
ALTER TABLE app.api_keys ADD COLUMN last_used_at timestamptz;
GRANT SELECT(id,status) ON app.tenants TO app_auth;
GRANT SELECT(id,status),UPDATE(status) ON app.tenants TO app_credential_writer;
GRANT SELECT(id,status) ON app.users TO app_credential_writer;
GRANT SELECT(tenant_id,user_id,role,status),UPDATE(status) ON app.tenant_memberships TO app_credential_writer;
GRANT SELECT(tenant_id,id,issued_by,key_prefix,key_hash,scopes,expires_at,revoked_at,created_at,last_used_at),
 UPDATE(revoked_at,last_used_at) ON app.api_keys TO app_credential_writer;
GRANT SELECT(id,user_id,expires_at,revoked_at,created_at,last_seen_at),UPDATE(revoked_at) ON app.auth_sessions TO app_credential_writer;
CREATE POLICY credential_key_read ON app.api_keys FOR SELECT TO app_credential_writer USING (true);

-- The role allowlist is deliberately explicit; unknown roles have no scopes.
-- This definition mirrors identity/contract.RoleScopes (contract gate compares).
CREATE FUNCTION app.auth_role_scopes(p_role text) RETURNS text[]
LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $scope$
 SELECT CASE p_role
 WHEN 'owner' THEN ARRAY['organization.create','site.create','catalog:read','catalog:write','media:write','cms:write','cms:publish','quotes:read','quotes:write','notifications:write','orders:read','apikeys:write']
 WHEN 'admin' THEN ARRAY['organization.create','site.create','catalog:read','catalog:write','media:write','cms:write','cms:publish','quotes:read','quotes:write','notifications:write','orders:read','apikeys:write']
 WHEN 'catalog_editor' THEN ARRAY['catalog:read','catalog:write','media:write']
 WHEN 'content_editor' THEN ARRAY['catalog:read','cms:write','cms:publish','media:write']
 WHEN 'viewer' THEN ARRAY['catalog:read'] ELSE ARRAY[]::text[] END;
$scope$;
REVOKE ALL ON FUNCTION app.auth_role_scopes(text) FROM PUBLIC;

-- Lock the shared authority prefix in the same order as password/session writes.
-- FALSE is the only invalid-authority result; no credential material is returned.
CREATE FUNCTION app.auth_revalidate_tenant(p_tenant uuid,p_user uuid,p_kind text,p_credential uuid,p_scope text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $authorize$
DECLARE u_status text; t_status text; m_status text; m_role text;
 s_user uuid; s_exp timestamptz; s_rev timestamptz; s_created timestamptz; s_seen timestamptz;
 k_tenant uuid; k_user uuid; k_scopes text[]; k_exp timestamptz; k_rev timestamptz; at_db timestamptz;
BEGIN
 IF p_kind IS NULL OR p_kind NOT IN ('session','api_key') OR p_scope IS NULL OR p_credential IS NULL
  OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RETURN false; END IF;
 SELECT status INTO u_status FROM app.users WHERE id=p_user FOR SHARE;
 IF NOT FOUND OR u_status<>'active' THEN RETURN false; END IF;
 SELECT status INTO t_status FROM app.tenants WHERE id=p_tenant FOR SHARE;
 IF NOT FOUND OR t_status<>'active' THEN RETURN false; END IF;
 SELECT role,status INTO m_role,m_status FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_user FOR SHARE;
 IF NOT FOUND OR m_status<>'active' OR NOT p_scope=ANY(app.auth_role_scopes(m_role)) THEN RETURN false; END IF;
 IF p_kind='session' THEN
  SELECT user_id,expires_at,revoked_at,created_at,last_seen_at INTO s_user,s_exp,s_rev,s_created,s_seen
   FROM app.auth_sessions WHERE id=p_credential FOR SHARE;
  at_db:=clock_timestamp();
  RETURN FOUND AND s_user=p_user AND s_rev IS NULL AND isfinite(s_exp) AND s_exp>at_db
   AND isfinite(s_created) AND isfinite(s_seen) AND s_created<=at_db AND s_seen<=at_db
   AND s_seen>=s_created AND s_exp>s_created;
 END IF;
 SELECT tenant_id,issued_by,scopes,expires_at,revoked_at INTO k_tenant,k_user,k_scopes,k_exp,k_rev
  FROM app.api_keys WHERE id=p_credential FOR SHARE;
 at_db:=clock_timestamp();
 RETURN FOUND AND k_tenant=p_tenant AND k_user=p_user AND k_rev IS NULL
  AND (k_exp IS NULL OR isfinite(k_exp) AND k_exp>at_db) AND p_scope=ANY(k_scopes);
END;
$authorize$;
REVOKE ALL ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) FROM PUBLIC;

CREATE FUNCTION app.auth_create_api_key(p_tenant uuid,p_user uuid,p_id uuid,p_prefix text,p_hash bytea,p_scopes text[],p_expires timestamptz)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $create$
DECLARE m_role text; m_status text; u_status text; t_status text; at_db timestamptz;
BEGIN
 SELECT status INTO u_status FROM app.users WHERE id=p_user FOR SHARE;
 IF NOT FOUND OR u_status<>'active' THEN RAISE EXCEPTION 'API key issuance denied' USING ERRCODE='42501'; END IF;
 SELECT status INTO t_status FROM app.tenants WHERE id=p_tenant FOR SHARE;
 IF NOT FOUND OR t_status<>'active' THEN RAISE EXCEPTION 'API key issuance denied' USING ERRCODE='42501'; END IF;
 SELECT role,status INTO m_role,m_status FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_user FOR SHARE;
 IF NOT FOUND OR m_status<>'active' OR m_role NOT IN ('owner','admin') THEN RAISE EXCEPTION 'API key issuance denied' USING ERRCODE='42501'; END IF;
 at_db:=clock_timestamp();
 IF p_id IS NULL OR p_prefix IS NULL OR length(p_prefix) NOT BETWEEN 1 AND 64 OR p_hash IS NULL OR octet_length(p_hash)<>32
  OR p_scopes IS NULL OR cardinality(p_scopes) NOT BETWEEN 0 AND 64 OR array_position(p_scopes,NULL) IS NOT NULL
  OR NOT p_scopes<@app.auth_role_scopes(m_role) OR p_expires IS NOT NULL AND (NOT isfinite(p_expires) OR p_expires<=at_db)
 THEN RAISE EXCEPTION 'API key input invalid' USING ERRCODE='22023'; END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true);
 INSERT INTO app.api_keys(tenant_id,id,issued_by,key_prefix,key_hash,scopes,expires_at)
  VALUES(p_tenant,p_id,p_user,p_prefix,p_hash,p_scopes,p_expires);
 RETURN p_id;
END;
$create$;
REVOKE ALL ON FUNCTION app.auth_create_api_key(uuid,uuid,uuid,text,bytea,text[],timestamptz) FROM PUBLIC;

CREATE FUNCTION app.auth_revoke_api_key(p_tenant uuid,p_user uuid,p_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $revoke$
DECLARE u_status text; t_status text; m_role text; m_status text;
BEGIN
 SELECT status INTO u_status FROM app.users WHERE id=p_user FOR SHARE;
 IF NOT FOUND OR u_status<>'active' THEN RETURN false; END IF;
 SELECT status INTO t_status FROM app.tenants WHERE id=p_tenant FOR SHARE;
 IF NOT FOUND OR t_status<>'active' THEN RETURN false; END IF;
 SELECT role,status INTO m_role,m_status FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_user FOR SHARE;
 IF NOT FOUND OR m_status<>'active' OR m_role NOT IN ('owner','admin') THEN RETURN false; END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true);
 UPDATE app.api_keys SET revoked_at=coalesce(revoked_at,clock_timestamp()) WHERE tenant_id=p_tenant AND id=p_id;
 RETURN FOUND;
END;
$revoke$;
REVOKE ALL ON FUNCTION app.auth_revoke_api_key(uuid,uuid,uuid) FROM PUBLIC;

CREATE FUNCTION app.auth_touch_api_key(p_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $touch$
DECLARE k_tenant uuid; k_user uuid; k_rev timestamptz; k_exp timestamptz;
 u_status text; t_status text; m_status text; at_db timestamptz;
BEGIN
 -- First read only locates the lock prefix; the key is reread after prefix locks.
 SELECT tenant_id,issued_by INTO k_tenant,k_user FROM app.api_keys WHERE id=p_id;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT status INTO u_status FROM app.users WHERE id=k_user FOR SHARE;
 IF NOT FOUND OR u_status<>'active' THEN RETURN false; END IF;
 SELECT status INTO t_status FROM app.tenants WHERE id=k_tenant FOR SHARE;
 IF NOT FOUND OR t_status<>'active' THEN RETURN false; END IF;
 SELECT status INTO m_status FROM app.tenant_memberships WHERE tenant_id=k_tenant AND user_id=k_user FOR SHARE;
 IF NOT FOUND OR m_status<>'active' THEN RETURN false; END IF;
 PERFORM set_config('app.tenant_id',k_tenant::text,true);
 SELECT revoked_at,expires_at INTO k_rev,k_exp FROM app.api_keys WHERE id=p_id AND tenant_id=k_tenant AND issued_by=k_user FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 at_db:=clock_timestamp();
 IF k_rev IS NOT NULL OR k_exp IS NOT NULL AND (NOT isfinite(k_exp) OR k_exp<=at_db) THEN RETURN false; END IF;
 UPDATE app.api_keys SET last_used_at=at_db WHERE id=p_id AND tenant_id=k_tenant;
 RETURN true;
END;
$touch$;
REVOKE ALL ON FUNCTION app.auth_touch_api_key(uuid) FROM PUBLIC;

GRANT CREATE ON SCHEMA app TO app_credential_writer;
ALTER FUNCTION app.auth_role_scopes(text) OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_create_api_key(uuid,uuid,uuid,text,bytea,text[],timestamptz) OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_revoke_api_key(uuid,uuid,uuid) OWNER TO app_credential_writer;
ALTER FUNCTION app.auth_touch_api_key(uuid) OWNER TO app_credential_writer;
REVOKE CREATE ON SCHEMA app FROM app_credential_writer;
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) TO app_runtime;
GRANT EXECUTE ON FUNCTION app.auth_create_api_key(uuid,uuid,uuid,text,bytea,text[],timestamptz),app.auth_revoke_api_key(uuid,uuid,uuid),app.auth_touch_api_key(uuid) TO app_auth;
