-- stable-id: mdel02-identity
-- depends-on: mdel01-lifecycle, h4-identity
-- module: identity
-- Responsibility: merchant proof custody and target-tenant Identity admission.
-- Dependencies: Tenant lifecycle public fence and authorization capabilities.
-- Invariants: lifecycle -> user/session -> proof; DB clock after locks; no global
-- account/session deletion, no activation or raw proof grants. Rollback: revert
-- code before landing; after landing use a new forward migration, never rewind.
DO $role$
BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_identity_merchant_executor') THEN
  CREATE ROLE app_identity_merchant_executor NOLOGIN;
 END IF;
 ALTER ROLE app_identity_merchant_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_identity_merchant_executor'::regrole OR roleid='app_identity_merchant_executor'::regrole) THEN
  RAISE EXCEPTION 'Identity merchant executor must be unassumable'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_identity_merchant_executor;
CREATE TABLE app.identity_merchant_proofs (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 user_id uuid NOT NULL REFERENCES app.users(id),
 session_id uuid NOT NULL REFERENCES app.auth_sessions(id) ON DELETE CASCADE,
 action text NOT NULL CHECK(action IN('request','cancel')),
 proof_digest bytea NOT NULL CHECK(octet_length(proof_digest)=32),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK(isfinite(expires_at) AND isfinite(created_at) AND expires_at=created_at+interval '5 minutes'),
 PRIMARY KEY(tenant_id,id),
 UNIQUE(tenant_id,session_id,action),
 UNIQUE(tenant_id,proof_digest)
);
ALTER TABLE app.identity_merchant_proofs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.identity_merchant_proofs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.identity_merchant_proofs
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT UPDATE(id) ON app.identity_merchant_proofs TO app_identity_merchant_executor;
GRANT SELECT,INSERT,DELETE ON app.identity_merchant_proofs TO app_identity_merchant_executor;
-- Locks require an UPDATE column privilege; no exposed function updates these
-- global rows. Password/session validity is rechecked after their shared locks.
GRANT SELECT(id,status,password_hash),UPDATE(id) ON app.users TO app_identity_merchant_executor;
GRANT SELECT(id,user_id,created_at,last_seen_at,expires_at,revoked_at),UPDATE(id) ON app.auth_sessions TO app_identity_merchant_executor;
GRANT SELECT(tenant_id,user_id,role,status) ON app.tenant_memberships TO app_identity_merchant_executor;
CREATE POLICY identity_merchant_membership_read ON app.tenant_memberships FOR SELECT TO app_identity_merchant_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.tenant_lifecycles,app.tenant_lifecycle_events,app.tenant_lifecycle_receipts TO app_identity_merchant_executor;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_read_fence(uuid),app.tenant_lifecycle_transition_fence(uuid),app.auth_revalidate_tenant(uuid,uuid,text,uuid,text)
 TO app_identity_merchant_executor;

-- Shared authorization surface, read-only over Tenant state. Both pre-request
-- admission and transaction revalidation use it; it never revokes global sessions.
CREATE FUNCTION app.auth_merchant_scope(p_tenant uuid,p_kind text,p_scope text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE st text; ph text;
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RETURN false; END IF;
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 SELECT state,phase INTO STRICT st,ph FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 IF st='active' THEN RETURN true; END IF;
 IF st='cooling_off' OR st='failed' AND ph IN('none','preconditions') THEN
  RETURN p_kind='session' AND p_scope IN('catalog:read','orders:read','quotes:read','tenant:delete');
 END IF;
 RETURN false;
END $fn$;

CREATE FUNCTION app.auth_issue_merchant_proof(p_tenant uuid,p_user uuid,p_session uuid,p_action text,p_digest bytea,p_verified_hash text)
RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE at_db timestamptz;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 IF p_action IS NULL OR p_action NOT IN('request','cancel') OR p_digest IS NULL OR octet_length(p_digest)<>32
  OR NOT app.auth_revalidate_tenant(p_tenant,p_user,'session',p_session,'tenant:delete')
  OR NOT EXISTS(SELECT FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_user AND role='owner' AND status='active')
  OR NOT EXISTS(SELECT FROM app.users WHERE id=p_user AND status='active' AND password_hash=p_verified_hash) THEN
  RAISE EXCEPTION 'merchant step-up denied' USING ERRCODE='42501'; END IF;
 at_db:=clock_timestamp();
 -- Replace expired digests and bound storage to one outstanding proof per
 -- session/action. No raw proof or password hash is retained.
 DELETE FROM app.identity_merchant_proofs WHERE tenant_id=p_tenant
  AND (expires_at<=at_db OR user_id=p_user AND session_id=p_session AND action=p_action);
 INSERT INTO app.identity_merchant_proofs(tenant_id,user_id,session_id,action,proof_digest,created_at,expires_at)
 VALUES(p_tenant,p_user,p_session,p_action,p_digest,at_db,at_db+interval '5 minutes');
 RETURN at_db+interval '5 minutes';
END $fn$;

-- No runtime/auth grant. The coordinator consumes this port in the exact
-- transaction that performs request/cancel. Replay bypass is decided only by
-- Tenant's persisted idempotency receipt after live session revalidation.
CREATE FUNCTION app.auth_consume_merchant_proof(p_tenant uuid,p_user uuid,p_session uuid,p_action text,p_digest bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE proof app.identity_merchant_proofs; at_db timestamptz;
BEGIN
 PERFORM app.tenant_lifecycle_transition_fence(p_tenant);
 IF NOT app.auth_revalidate_tenant(p_tenant,p_user,'session',p_session,'tenant:delete')
  OR NOT EXISTS(SELECT FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_user AND role='owner' AND status='active') THEN
  RAISE EXCEPTION 'merchant step-up denied' USING ERRCODE='42501'; END IF;
 SELECT * INTO proof FROM app.identity_merchant_proofs WHERE tenant_id=p_tenant AND proof_digest=p_digest
  AND user_id=p_user AND session_id=p_session AND action=p_action FOR UPDATE;
 at_db:=clock_timestamp();
 IF NOT FOUND OR proof.expires_at<=at_db THEN RAISE EXCEPTION 'merchant step-up denied' USING ERRCODE='42501'; END IF;
 DELETE FROM app.identity_merchant_proofs WHERE tenant_id=p_tenant AND id=proof.id;
END $fn$;

-- Minimal boolean authority for the receipt-only route. Membership is purposely
-- absent after detachment; no general tenant scope or business data is granted.
CREATE FUNCTION app.auth_merchant_receipt(p_tenant uuid,p_request uuid,p_user uuid,p_session uuid) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE at_db timestamptz; sess record;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 PERFORM id FROM app.users WHERE id=p_user AND status='active' FOR SHARE;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT id,user_id,created_at,last_seen_at,expires_at,revoked_at INTO sess FROM app.auth_sessions WHERE id=p_session AND user_id=p_user FOR SHARE;
 at_db:=clock_timestamp();
 IF NOT FOUND OR sess.revoked_at IS NOT NULL OR NOT isfinite(sess.expires_at) OR sess.expires_at<=at_db
  OR NOT isfinite(sess.created_at) OR NOT isfinite(sess.last_seen_at) OR sess.created_at>at_db OR sess.last_seen_at>at_db
  OR sess.last_seen_at<sess.created_at OR sess.expires_at<=sess.created_at THEN RETURN false; END IF;
 RETURN EXISTS(SELECT FROM app.tenant_lifecycle_events e JOIN app.tenant_lifecycle_receipts r
  ON r.tenant_id=e.tenant_id AND r.request_id=e.request_id AND r.operation='request'
  WHERE e.tenant_id=p_tenant AND e.request_id=p_request AND e.state IN('canceled','deleted') AND r.actor_id=p_user);
END $fn$;

CREATE FUNCTION app.auth_erase_merchant_proofs(p_tenant uuid,p_request uuid,p_epoch bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE n bigint;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 IF NOT EXISTS(SELECT FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant
  AND request_id=p_request AND epoch=p_epoch AND state='executing' AND phase='executing') THEN
  RAISE EXCEPTION 'stale Identity erasure' USING ERRCODE='55000'; END IF;
 DELETE FROM app.identity_merchant_proofs WHERE tenant_id=p_tenant;
 GET DIAGNOSTICS n=ROW_COUNT;
 RETURN n;
END $fn$;

-- Preserve the existing authorization body, owner and ACL. The common fence
-- precedes its global/user locks; existing unknown scope checks stay intact.
DO $wrap$
DECLARE def text; body text;
BEGIN
 SELECT pg_get_functiondef(p.oid),p.prosrc INTO STRICT def,body FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname='auth_revalidate_tenant';
 EXECUTE replace(def,body,regexp_replace(body,'\mBEGIN\M',
  'BEGIN IF NOT app.auth_merchant_scope(p_tenant,p_kind,p_scope) THEN RETURN false; END IF;'));
END $wrap$;
-- Keep Tenant's existing invalid-destructive-cancel conflict classification.
-- Identity now fences interactive authority in that phase, so reject the
-- impossible transition before owner revalidation. This opens no authority:
-- the primitive stays ungranted and request/normal-cancel checks are unchanged.
DO $phase_guard$
DECLARE def text; body text;
BEGIN
 SELECT pg_get_functiondef(p.oid),p.prosrc INTO STRICT def,body FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname='tenant_lifecycle_transition';
 EXECUTE replace(def,body,replace(body,'now_db:=clock_timestamp();',
  'now_db:=clock_timestamp(); IF p_operation=''cancel'' AND l.phase=''executing'' THEN RAISE EXCEPTION ''tenant deletion cancellation closed'' USING ERRCODE=''55000''; END IF;'));
END $phase_guard$;
DO $owners$
DECLARE f record;
BEGIN
 GRANT CREATE ON SCHEMA app TO app_identity_merchant_executor;
 FOR f IN SELECT p.oid::regprocedure signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname IN('auth_merchant_scope','auth_issue_merchant_proof','auth_consume_merchant_proof','auth_merchant_receipt','auth_erase_merchant_proofs') LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.signature);
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_identity_merchant_executor',f.signature);
 END LOOP;
 REVOKE CREATE ON SCHEMA app FROM app_identity_merchant_executor;
END $owners$;
GRANT EXECUTE ON FUNCTION app.auth_merchant_scope(uuid,text,text) TO app_auth,app_credential_writer;
GRANT EXECUTE ON FUNCTION app.auth_issue_merchant_proof(uuid,uuid,uuid,text,bytea,text),app.auth_merchant_receipt(uuid,uuid,uuid,uuid) TO app_auth;
GRANT EXECUTE ON FUNCTION app.auth_merchant_receipt(uuid,uuid,uuid,uuid) TO app_runtime;
-- Consume/erase remain ungranted until restricted DataRights composition.
