-- stable-id: mdel01-lifecycle
-- depends-on: mdel00-caller-keys
-- depends-on: c22a-data-rights
-- depends-on: h4-identity
-- Owner: Tenant. Lifecycle metadata is not the DataRights request/worklist.
-- Admission is disabled: no login/runtime/auth role can execute transitions or
-- drains. MDEL-02/24/26 must compose proof, obligations, serving and worklist gates
-- in the SAME transaction before granting a narrowly authorized entry point.
-- Lock order: lifecycle row -> Identity authority -> module locks. All times are
-- sampled in DB after the lifecycle lock. Forward upgrade preserves business rows.
DO $scopes$
DECLARE inherited text;
BEGIN
 SELECT regexp_replace(p.prosrc,'[[:space:];]+$','') INTO STRICT inherited
 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
 WHERE p.oid='app.auth_role_scopes(text)'::regprocedure AND l.lanname='sql'
  AND p.provolatile='i' AND NOT p.prosecdef;
 EXECUTE format('CREATE OR REPLACE FUNCTION app.auth_role_scopes(p_role text) RETURNS text[]
 LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS %L',
 'SELECT ARRAY(SELECT scope FROM (SELECT unnest(('||inherited||')) AS scope
 UNION SELECT ''tenant:delete'' WHERE p_role=''owner'') scopes ORDER BY scope)');
END $scopes$;

DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_tenant_lifecycle_executor') THEN
  CREATE ROLE app_tenant_lifecycle_executor NOLOGIN;
 END IF;
 ALTER ROLE app_tenant_lifecycle_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_tenant_lifecycle_executor'::regrole OR roleid='app_tenant_lifecycle_executor'::regrole) THEN
  RAISE EXCEPTION 'MDEL01_EXECUTOR_MUST_BE_UNASSUMABLE'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_tenant_lifecycle_executor;

-- Empty tenants retain their existing removal contract. A lifecycle with any
-- request/event cannot cascade away: the child composite FKs stay restrictive.
CREATE TABLE app.tenant_lifecycles (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id) ON DELETE CASCADE,
 id uuid NOT NULL CHECK(id=tenant_id),
 state text NOT NULL DEFAULT 'active' CHECK(state IN('active','cooling_off','executing','failed','deleted')),
 epoch bigint NOT NULL DEFAULT 0 CHECK(epoch>=0),
 request_id uuid,
 requester_id uuid,
 requested_at timestamptz,
 execute_after timestamptz,
 phase text NOT NULL DEFAULT 'none' CHECK(phase IN('none','preconditions','executing','completed')),
 attempt integer NOT NULL DEFAULT 0 CHECK(attempt>=0),
 next_retry_at timestamptz,
 completed_at timestamptz,
 PRIMARY KEY(tenant_id,id),
 CHECK((requested_at IS NULL)=(execute_after IS NULL)),
 CHECK(execute_after IS NULL OR isfinite(execute_after) AND isfinite(requested_at) AND execute_after>=requested_at),
 CHECK(state IN('active','deleted') OR request_id IS NOT NULL AND requester_id IS NOT NULL AND requested_at IS NOT NULL)
);
CREATE TABLE app.tenant_lifecycle_receipts (
 tenant_id uuid NOT NULL,
 lifecycle_id uuid NOT NULL CHECK(lifecycle_id=tenant_id),
 id uuid NOT NULL,
 request_id uuid NOT NULL,
 actor_id uuid NOT NULL,
 operation text NOT NULL CHECK(operation IN('request','cancel')),
 body_digest bytea NOT NULL CHECK(octet_length(body_digest)=32),
 metadata jsonb NOT NULL CHECK(jsonb_typeof(metadata)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,lifecycle_id) REFERENCES app.tenant_lifecycles(tenant_id,id)
);
CREATE TABLE app.tenant_lifecycle_events (
 tenant_id uuid NOT NULL,
 lifecycle_id uuid NOT NULL CHECK(lifecycle_id=tenant_id),
 id uuid NOT NULL DEFAULT uuidv7(),
 request_id uuid NOT NULL,
 epoch bigint NOT NULL,
 state text NOT NULL CHECK(state IN('requested','cooling_off','canceled','executing','failed','deleted')),
 phase text NOT NULL CHECK(phase IN('none','preconditions','executing','completed')),
 blockers jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),
 UNIQUE(tenant_id,request_id,epoch,state),
 FOREIGN KEY(tenant_id,lifecycle_id) REFERENCES app.tenant_lifecycles(tenant_id,id)
);
DO $rls$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['tenant_lifecycles','tenant_lifecycle_receipts','tenant_lifecycle_events'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I USING (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK (tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime,app_auth,app_customer_auth,app_domain_resolver',t);
 END LOOP;
END $rls$;
GRANT SELECT ON app.tenant_lifecycles TO app_runtime;
GRANT SELECT,INSERT,UPDATE ON app.tenant_lifecycles TO app_tenant_lifecycle_executor;
GRANT SELECT,INSERT ON app.tenant_lifecycle_receipts,app.tenant_lifecycle_events TO app_tenant_lifecycle_executor;
GRANT SELECT(id,status) ON app.tenants TO app_tenant_lifecycle_executor;
GRANT SELECT(tenant_id,user_id,role,status) ON app.tenant_memberships TO app_tenant_lifecycle_executor;
CREATE POLICY lifecycle_membership_read ON app.tenant_memberships FOR SELECT TO app_tenant_lifecycle_executor
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text),app.auth_role_scopes(text) TO app_tenant_lifecycle_executor;

INSERT INTO app.tenant_lifecycles(tenant_id,id,state,completed_at)
 SELECT id,id,CASE WHEN status='deleted' THEN 'deleted' ELSE 'active' END,
 CASE WHEN status='deleted' THEN clock_timestamp() END FROM app.tenants;

CREATE FUNCTION app.tenant_lifecycle_initialize() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE previous text:=current_setting('app.tenant_id',true);
BEGIN
 PERFORM set_config('app.tenant_id',NEW.id::text,true);
 INSERT INTO app.tenant_lifecycles(tenant_id,id,state,completed_at)
 VALUES(NEW.id,NEW.id,CASE WHEN NEW.status='deleted' THEN 'deleted' ELSE 'active' END,
  CASE WHEN NEW.status='deleted' THEN clock_timestamp() END);
 PERFORM set_config('app.tenant_id',coalesce(previous,''),true);
 RETURN NEW;
END $fn$;
CREATE TRIGGER tenant_lifecycle_initialize AFTER INSERT ON app.tenants
 FOR EACH ROW EXECUTE FUNCTION app.tenant_lifecycle_initialize();

-- Common read admission retains SHARE until transaction end. It permits staff
-- reads during cooling; mutation admission is a separate, fail-closed check.
CREATE FUNCTION app.tenant_lifecycle_read_fence(p_tenant uuid) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE e bigint;
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'tenant context required' USING ERRCODE='42501'; END IF;
 SELECT epoch INTO STRICT e FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant FOR SHARE;
 RETURN e;
END $fn$;
CREATE FUNCTION app.tenant_lifecycle_write_fence(p_tenant uuid) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE e bigint; s text;
BEGIN
 e:=app.tenant_lifecycle_read_fence(p_tenant);
 SELECT state INTO STRICT s FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 IF s<>'active' THEN RAISE EXCEPTION 'tenant deletion write fence' USING ERRCODE='55000'; END IF;
 RETURN e;
END $fn$;
-- Only owner ports completing existing obligations may receive this capability
-- in their own cards. An epoch is an admission witness, never caller authority.
CREATE FUNCTION app.tenant_lifecycle_drain_fence(p_tenant uuid,p_epoch bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE e bigint; s text; ph text;
BEGIN
 e:=app.tenant_lifecycle_read_fence(p_tenant);
 SELECT state,phase INTO STRICT s,ph FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 IF e IS DISTINCT FROM p_epoch OR s NOT IN('active','cooling_off','failed') OR s='failed' AND ph<>'preconditions' THEN
  RAISE EXCEPTION 'tenant deletion drain fence' USING ERRCODE='55000'; END IF;
 RETURN e;
END $fn$;

-- The coordinator must acquire this exclusive prefix BEFORE collecting owner
-- obligations through the transaction-scoped port. Only then call transition.
-- No caller role receives this capability until the all-module activation gate.
CREATE FUNCTION app.tenant_lifecycle_transition_fence(p_tenant uuid) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE e bigint;
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'tenant context required' USING ERRCODE='42501'; END IF;
 SELECT epoch INTO STRICT e FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant FOR UPDATE;
 RETURN e;
END $fn$;

-- All callers are trusted transaction-scoped owner ports; there is deliberately
-- no runtime/auth EXECUTE grant. Identity consumes step-up in the same transaction
-- and DataRights composes serving/export/worklist gates before activation.
CREATE FUNCTION app.tenant_lifecycle_transition(p_tenant uuid,p_request uuid,p_actor uuid,p_session uuid,
 p_operation text,p_key uuid,p_digest bytea,p_epoch bigint,p_obligations jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE l app.tenant_lifecycles; old app.tenant_lifecycle_receipts; now_db timestamptz;
 result jsonb; category text; blocked boolean:=false; details jsonb:='{}';
 categories text[]:=ARRAY['paid_unfulfilled','fulfillment_return','payment_refund','checkout_capture','refund_reconciliation','pending_payout','unsettled_provider_balance','legal_hold'];
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'tenant context required' USING ERRCODE='42501'; END IF;
 PERFORM app.tenant_lifecycle_transition_fence(p_tenant);
 SELECT * INTO STRICT l FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 now_db:=clock_timestamp();
 IF p_operation IS NULL OR p_operation NOT IN('request','cancel','execute') OR p_request IS NULL THEN
  RAISE EXCEPTION 'invalid lifecycle operation' USING ERRCODE='22023'; END IF;
 IF p_operation IN('request','cancel') THEN
  IF NOT app.auth_revalidate_tenant(p_tenant,p_actor,'session',p_session,'tenant:delete')
   OR NOT EXISTS(SELECT FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_actor AND role='owner' AND status='active') THEN
   RAISE EXCEPTION 'owner session required' USING ERRCODE='42501'; END IF;
  IF p_key IS NULL OR p_digest IS NULL OR octet_length(p_digest)<>32 THEN
   RAISE EXCEPTION 'idempotency key and digest required' USING ERRCODE='22023'; END IF;
  SELECT * INTO old FROM app.tenant_lifecycle_receipts WHERE tenant_id=p_tenant AND id=p_key;
  IF FOUND THEN
   IF old.actor_id IS DISTINCT FROM p_actor OR old.operation IS DISTINCT FROM p_operation
    OR old.request_id IS DISTINCT FROM p_request OR old.body_digest IS DISTINCT FROM p_digest THEN
    RAISE EXCEPTION 'idempotency conflict' USING ERRCODE='23505'; END IF;
   RETURN old.metadata;
  END IF;
 END IF;
 IF p_operation='request' THEN
  IF l.state<>'active' THEN RAISE EXCEPTION 'tenant deletion already active' USING ERRCODE='55000'; END IF;
 ELSE
  IF l.request_id IS DISTINCT FROM p_request OR l.epoch IS DISTINCT FROM p_epoch THEN
   RAISE EXCEPTION 'stale lifecycle operation' USING ERRCODE='55000'; END IF;
 END IF;
 IF p_operation IN('request','execute') THEN
  IF jsonb_typeof(p_obligations) IS DISTINCT FROM 'object' THEN p_obligations:='{}'; END IF;
  IF EXISTS(SELECT FROM jsonb_object_keys(p_obligations) k WHERE NOT k=ANY(categories)) THEN
   RAISE EXCEPTION 'unknown obligation category' USING ERRCODE='22023'; END IF;
  FOREACH category IN ARRAY categories LOOP
   IF NOT p_obligations ? category OR jsonb_typeof(p_obligations->category)<>'number'
    OR (p_obligations->>category)!~'^[0-9]{1,18}$' THEN
    details:=details||jsonb_build_object(category,'unknown'); blocked:=true;
   ELSIF (p_obligations->>category)::bigint<>0 THEN
    details:=details||jsonb_build_object(category,(p_obligations->>category)::bigint); blocked:=true;
   END IF;
  END LOOP;
 END IF;
 IF p_operation='request' AND blocked THEN
  RETURN jsonb_build_object('code','tenant_deletion_blocked','categories',details);
 END IF;
 IF p_operation='request' THEN
  l.state:='cooling_off'; l.phase:='none'; l.epoch:=l.epoch+1;
  l.request_id:=p_request; l.requester_id:=p_actor;
  l.requested_at:=now_db; l.execute_after:=now_db+interval '720 hours'; l.attempt:=0;
  INSERT INTO app.tenant_lifecycle_events(tenant_id,lifecycle_id,request_id,epoch,state,phase,created_at)
   VALUES(p_tenant,p_tenant,p_request,l.epoch,'requested','none',now_db);
 ELSIF p_operation='cancel' THEN
  IF l.state NOT IN('cooling_off','failed') OR l.phase NOT IN('none','preconditions') OR now_db>=l.execute_after THEN
   RAISE EXCEPTION 'tenant deletion cancellation closed' USING ERRCODE='55000'; END IF;
  l.state:='active'; l.phase:='none'; l.epoch:=l.epoch+1;
 ELSE
  IF l.state NOT IN('cooling_off','failed') OR l.phase NOT IN('none','preconditions','executing') OR now_db<l.execute_after
   OR l.next_retry_at IS NOT NULL AND now_db<l.next_retry_at THEN
   RAISE EXCEPTION 'tenant deletion execution closed' USING ERRCODE='55000'; END IF;
  l.epoch:=l.epoch+1; l.attempt:=l.attempt+1;
  IF blocked THEN l.state:='failed';
   IF l.phase<>'executing' THEN l.phase:='preconditions'; END IF;
  ELSE l.state:='executing'; l.phase:='executing'; END IF;
 END IF;
 l.next_retry_at:=CASE WHEN l.state='failed' THEN now_db+interval '60 seconds' END;
 UPDATE app.tenant_lifecycles SET state=l.state,epoch=l.epoch,request_id=l.request_id,requester_id=l.requester_id,
  requested_at=l.requested_at,execute_after=l.execute_after,phase=l.phase,attempt=l.attempt,next_retry_at=l.next_retry_at
 WHERE tenant_id=p_tenant AND id=p_tenant;
 INSERT INTO app.tenant_lifecycle_events(tenant_id,lifecycle_id,request_id,epoch,state,phase,blockers,created_at)
 VALUES(p_tenant,p_tenant,p_request,l.epoch,CASE WHEN p_operation='cancel' THEN 'canceled' ELSE l.state END,l.phase,details,now_db);
 result:=jsonb_build_object('request_id',l.request_id,'state',l.state,'phase',l.phase,'epoch',l.epoch,
  'requested_at',l.requested_at,'execute_after',l.execute_after,'attempt',l.attempt,'next_retry_at',l.next_retry_at,'categories',details);
 IF p_operation IN('request','cancel') THEN
  INSERT INTO app.tenant_lifecycle_receipts(tenant_id,lifecycle_id,id,request_id,actor_id,operation,body_digest,metadata,created_at)
   VALUES(p_tenant,p_tenant,p_key,p_request,p_actor,p_operation,p_digest,result,now_db);
 END IF;
 RETURN result;
END $fn$;

-- Restricted worker completion/failure port. DataRights owns the durable
-- worklist/lease and all verification gates; this primitive only CASes Tenant's
-- saved execution phase. Like transition it is unreachable by runtime roles.
CREATE FUNCTION app.tenant_lifecycle_checkpoint(p_tenant uuid,p_request uuid,p_epoch bigint,p_completed boolean) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE l app.tenant_lifecycles; now_db timestamptz;
BEGIN
 PERFORM app.tenant_lifecycle_transition_fence(p_tenant);
 SELECT * INTO STRICT l FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 now_db:=clock_timestamp();
 IF l.request_id IS DISTINCT FROM p_request OR l.epoch IS DISTINCT FROM p_epoch
  OR l.state<>'executing' OR l.phase<>'executing' OR p_completed IS NULL THEN
  RAISE EXCEPTION 'stale lifecycle checkpoint' USING ERRCODE='55000'; END IF;
 l.epoch:=l.epoch+1;
 IF p_completed THEN l.state:='deleted'; l.phase:='completed'; l.completed_at:=now_db; l.next_retry_at:=NULL;
 ELSE l.state:='failed'; l.next_retry_at:=now_db+interval '60 seconds'; END IF;
 UPDATE app.tenant_lifecycles SET state=l.state,phase=l.phase,epoch=l.epoch,
  completed_at=l.completed_at,next_retry_at=l.next_retry_at WHERE tenant_id=p_tenant AND id=p_tenant;
 INSERT INTO app.tenant_lifecycle_events(tenant_id,lifecycle_id,request_id,epoch,state,phase,created_at)
  VALUES(p_tenant,p_tenant,p_request,l.epoch,l.state,l.phase,now_db);
 RETURN l.epoch;
END $fn$;

-- Tenant-owned legacy SQL entries must take the common prefix before their
-- pre-existing user/membership/key locks. Preserve their full implementations,
-- owners and ACLs; only prepend admission, never replace their authorization.
DO $wrap$
DECLARE name text; oid_value regprocedure; definition text; body text; prefix text;
BEGIN
 FOREACH name IN ARRAY ARRAY['auth_revalidate_tenant','auth_create_api_key','auth_revoke_api_key','auth_touch_api_key'] LOOP
  SELECT p.oid::regprocedure,pg_get_functiondef(p.oid),p.prosrc INTO STRICT oid_value,definition,body
   FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname=name;
  IF name='auth_touch_api_key' THEN
   -- Locate the trusted key tenant without locks, then admit before Identity
   -- and key locks. Keep the existing validity reread and SKIP LOCKED throttle.
   prefix:='IF NOT FOUND THEN RETURN false; END IF; PERFORM set_config(''app.tenant_id'',k_tenant::text,true); PERFORM app.tenant_lifecycle_write_fence(k_tenant);';
   EXECUTE replace(definition,body,regexp_replace(body,'IF NOT FOUND THEN RETURN false; END IF;',prefix));
   CONTINUE;
  END IF;
  prefix:='BEGIN ';
  IF name<>'auth_revalidate_tenant' THEN
   prefix:=prefix||' PERFORM set_config(''app.tenant_id'',p_tenant::text,true); PERFORM app.tenant_lifecycle_write_fence(p_tenant);';
  ELSE
   prefix:=prefix||' IF p_tenant IS DISTINCT FROM nullif(current_setting(''app.tenant_id'',true),'''')::uuid THEN RETURN false; END IF; PERFORM app.tenant_lifecycle_read_fence(p_tenant);';
  END IF;
  EXECUTE replace(definition,body,regexp_replace(body,'\mBEGIN\M',prefix));
 END LOOP;
END $wrap$;

DO $owners$
DECLARE f record;
BEGIN
 GRANT CREATE ON SCHEMA app TO app_tenant_lifecycle_executor;
 FOR f IN SELECT p.oid::regprocedure signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname LIKE 'tenant_lifecycle_%' LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.signature);
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_tenant_lifecycle_executor',f.signature);
 END LOOP;
 REVOKE CREATE ON SCHEMA app FROM app_tenant_lifecycle_executor;
END $owners$;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_read_fence(uuid) TO app_runtime,app_auth,app_credential_writer;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_write_fence(uuid) TO app_runtime,app_credential_writer;
-- No transition/drain grant: activation is intentionally unreachable.
