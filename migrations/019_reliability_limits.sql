-- Responsibility: finite outbox retries, bounded claim TTL and IDs-only discovery.
-- Dependencies: frozen 007/008/011; caller: forward-only migration runner,
-- idempotency sqlc adapter, outbox worker. Invariants: no eleventh claim,
-- expired final leases become terminal; original owner/attempt/clock fences remain.
-- Upgrade: terminal delivery records remain intact; exhausted legacy work is
-- retired on its next tenant claim. Rollback: forward repair; migration is atomic.
ALTER TABLE app.outbox_events DROP CONSTRAINT outbox_events_status_check;
ALTER TABLE app.outbox_events ADD CONSTRAINT outbox_events_status_check
 CHECK (status IN ('pending','processing','delivered','failed','dead_letter'));
ALTER TABLE app.outbox_events DROP CONSTRAINT outbox_lease_shape;
ALTER TABLE app.outbox_events ADD CONSTRAINT outbox_lease_shape CHECK (
 (status='processing' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL AND attempts>0 AND delivered_at IS NULL)
 OR (status IN ('pending','failed','dead_letter') AND lease_owner IS NULL AND lease_until IS NULL AND delivered_at IS NULL)
 OR (status='delivered' AND lease_owner IS NULL AND lease_until IS NULL AND delivered_at IS NOT NULL));

CREATE OR REPLACE FUNCTION app.idempotency_claim(p_tenant uuid,p_actor_kind text,p_actor_id uuid,p_operation text,
 p_key text,p_hash bytea,p_ttl interval,p_claim uuid) RETURNS app.idempotency_records
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE r app.idempotency_records; ttl_seconds numeric:=extract(epoch FROM p_ttl); BEGIN
 -- Interval comparison normalizes years differently from epoch extraction.
 -- Validate the exact elapsed seconds used below, including mixed components.
 IF p_ttl IS NULL OR NOT isfinite(p_ttl) OR ttl_seconds<=0 OR ttl_seconds>86400 THEN
  RAISE EXCEPTION 'IDEMPOTENCY_TTL_ARGUMENT' USING ERRCODE='22023';
 END IF;
 -- Interpret the bounded interval as elapsed seconds, including across DST.
 INSERT INTO app.idempotency_records AS existing(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(p_tenant,p_actor_kind,p_actor_id,p_operation,p_key,p_hash,
  clock_timestamp()+make_interval(secs=>ttl_seconds),p_claim)
 ON CONFLICT(tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET
  claim_token=CASE WHEN existing.state='in_progress' AND existing.expires_at<=clock_timestamp() AND existing.request_hash=EXCLUDED.request_hash
                   THEN EXCLUDED.claim_token ELSE existing.claim_token END,
  expires_at=CASE WHEN existing.state='in_progress' AND existing.expires_at<=clock_timestamp() AND existing.request_hash=EXCLUDED.request_hash
                  THEN EXCLUDED.expires_at ELSE existing.expires_at END
 RETURNING existing.* INTO r;
 RETURN r;
END $$;

CREATE OR REPLACE FUNCTION app.claim_outbox(p_tenant uuid,p_worker uuid,p_limit integer,p_lease_seconds integer DEFAULT 60)
RETURNS SETOF app.outbox_events LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_SCOPE' USING ERRCODE='42501';
 END IF;
 IF p_worker IS NULL OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000
 OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 1 AND 3600 THEN
  RAISE EXCEPTION 'OUTBOX_CLAIM_ARGUMENT' USING ERRCODE='22023';
 END IF;
 -- Reap a crashed final attempt without issuing a new delivery epoch. SKIP
 -- LOCKED preserves competing workers' independence, including this cleanup.
 WITH exhausted AS MATERIALIZED (
  SELECT e.tenant_id,e.id FROM app.outbox_events e WHERE e.tenant_id=p_tenant AND e.attempts>=10
   AND (e.status IN ('pending','failed') OR (e.status='processing' AND e.lease_until<=clock_timestamp()))
  ORDER BY e.next_attempt_at,e.id LIMIT p_limit FOR UPDATE SKIP LOCKED
 ) UPDATE app.outbox_events e SET status='dead_letter',lease_owner=NULL,lease_until=NULL,
  last_error=coalesce(e.last_error,'OUTBOX_ATTEMPT_LIMIT')
  FROM exhausted WHERE e.tenant_id=exhausted.tenant_id AND e.id=exhausted.id;
 RETURN QUERY WITH due AS MATERIALIZED (
  SELECT e.tenant_id,e.id FROM app.outbox_events e WHERE e.tenant_id=p_tenant AND e.attempts<10
   AND ((e.status IN ('pending','failed') AND e.next_attempt_at<=clock_timestamp())
        OR (e.status='processing' AND e.lease_until<=clock_timestamp()))
  ORDER BY e.next_attempt_at,e.id LIMIT p_limit FOR UPDATE SKIP LOCKED
 ) UPDATE app.outbox_events e SET status='processing',lease_owner=p_worker,
  lease_until=clock_timestamp()+make_interval(secs=>p_lease_seconds),attempts=e.attempts+1,last_error=NULL
  FROM due WHERE e.tenant_id=due.tenant_id AND e.id=due.id RETURNING e.*;
END $$;

CREATE OR REPLACE FUNCTION app.fail_outbox(p_tenant uuid,p_id uuid,p_worker uuid,p_attempt integer,p_error text,p_retry_seconds integer DEFAULT 60)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ DECLARE e app.outbox_events; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_SCOPE' USING ERRCODE='42501';
 END IF;
 IF p_error IS NULL OR length(p_error) NOT BETWEEN 1 AND 2048 OR p_retry_seconds IS NULL OR p_retry_seconds NOT BETWEEN 0 AND 86400 THEN
  RAISE EXCEPTION 'OUTBOX_FAIL_ARGUMENT' USING ERRCODE='22023';
 END IF;
 SELECT * INTO e FROM app.outbox_events WHERE tenant_id=p_tenant AND id=p_id FOR UPDATE;
 IF NOT FOUND OR e.status<>'processing' OR e.lease_owner IS DISTINCT FROM p_worker
 OR e.attempts IS DISTINCT FROM p_attempt OR e.lease_until<=clock_timestamp() THEN RETURN false; END IF;
 UPDATE app.outbox_events SET status=CASE WHEN e.attempts>=10 THEN 'dead_letter' ELSE 'failed' END,
  lease_owner=NULL,lease_until=NULL,last_error=p_error,
  next_attempt_at=clock_timestamp()+make_interval(secs=>p_retry_seconds) WHERE tenant_id=p_tenant AND id=p_id;
 RETURN true;
END $$;

-- The broker is unreachable by SET ROLE and sees only the two discovery
-- columns. FORCE RLS remains enabled; its one cross-tenant policy is SELECT-only.
DO $$ DECLARE r record; BEGIN
 -- Roles are cluster-wide; disposable upgrade gates create several databases
 -- on one cluster. Reuse only an already restricted, unreachable identity.
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_outbox_tenant_broker') THEN
  CREATE ROLE app_outbox_tenant_broker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 END IF;
 SELECT * INTO r FROM pg_roles WHERE rolname='app_outbox_tenant_broker';
 IF r.rolcanlogin OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolinherit
 OR EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid OR roleid=r.oid) THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_BROKER_UNSAFE' USING ERRCODE='55000';
 END IF;
END $$;
GRANT USAGE ON SCHEMA app TO app_outbox_tenant_broker;
GRANT SELECT(tenant_id,status) ON app.outbox_events TO app_outbox_tenant_broker;
CREATE POLICY outbox_tenant_discovery ON app.outbox_events FOR SELECT TO app_outbox_tenant_broker USING (true);
CREATE FUNCTION app.outbox_active_tenants(p_after uuid,p_limit integer DEFAULT 100)
RETURNS TABLE(tenant_id uuid) LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_LIMIT_ARGUMENT' USING ERRCODE='22023';
 END IF;
 RETURN QUERY SELECT DISTINCT e.tenant_id FROM app.outbox_events e
  WHERE e.status IN ('pending','processing','failed') AND (p_after IS NULL OR e.tenant_id>p_after)
  ORDER BY e.tenant_id LIMIT p_limit;
END $$;
GRANT CREATE ON SCHEMA app TO app_outbox_tenant_broker;
ALTER FUNCTION app.outbox_active_tenants(uuid,integer) OWNER TO app_outbox_tenant_broker;
REVOKE CREATE ON SCHEMA app FROM app_outbox_tenant_broker;
REVOKE ALL ON FUNCTION app.outbox_active_tenants(uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.outbox_active_tenants(uuid,integer) TO app_outbox_worker;
