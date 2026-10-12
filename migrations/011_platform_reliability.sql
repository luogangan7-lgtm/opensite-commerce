-- Responsibility: tenant-scoped inbox, atomic request ownership and fenced outbox.
-- Dependencies: frozen 001..010; called by the forward-only migration runner.
-- Invariants: payload references are opaque; worker capabilities cannot rewrite
-- events; lease epochs are monotonic; request identity/results are immutable.
-- Upgrade: pending outbox rows receive legacy target + their existing ID as dedup
-- key. Ambiguous legacy results/leases fail closed; reconcile before retrying.
-- Rollback: forward repair only. This migration and its preflight are atomic.
DO $$ BEGIN
 IF EXISTS(SELECT FROM app.outbox_events WHERE status IN ('processing','delivered'))
 OR EXISTS(SELECT FROM app.idempotency_records WHERE state<>'in_progress'
           OR response_status IS NOT NULL OR response_body IS NOT NULL) THEN
  RAISE EXCEPTION 'RELIABILITY_UPGRADE_REQUIRES_RECONCILIATION: legacy leases/results need explicit reconciliation or response externalization' USING ERRCODE='55000';
 END IF;
END $$;

CREATE TABLE app.webhook_inbox (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 128),
 provider_account_id text NOT NULL CHECK (length(provider_account_id) BETWEEN 1 AND 256),
 environment text NOT NULL CHECK (environment IN ('test','live')),
 provider_event_id text NOT NULL CHECK (length(provider_event_id) BETWEEN 1 AND 256),
 payload_hash bytea NOT NULL CHECK (octet_length(payload_hash)=32),
 payload_reference text NOT NULL CHECK (length(payload_reference) BETWEEN 1 AND 2048),
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','processed','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 processed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,provider,provider_account_id,environment,provider_event_id),
 CHECK ((status='processed')=(processed_at IS NOT NULL))
);
ALTER TABLE app.webhook_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.webhook_inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.webhook_inbox
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.webhook_inbox FROM PUBLIC;
GRANT SELECT ON app.webhook_inbox TO app_runtime;
GRANT INSERT(tenant_id,id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference,created_at)
 ON app.webhook_inbox TO app_runtime;
GRANT UPDATE(status,attempts,next_attempt_at,processed_at) ON app.webhook_inbox TO app_runtime;

ALTER TABLE app.outbox_events
 ADD COLUMN delivery_target text NOT NULL DEFAULT 'legacy' CHECK (length(delivery_target) BETWEEN 1 AND 2048),
 ADD COLUMN dedup_key text,
 ADD COLUMN lease_owner uuid,
 ADD COLUMN lease_until timestamptz,
 ADD COLUMN last_error text CHECK (length(last_error)<=2048),
 ADD COLUMN delivered_at timestamptz;
UPDATE app.outbox_events SET dedup_key=id::text;
ALTER TABLE app.outbox_events ALTER COLUMN dedup_key SET DEFAULT uuidv7()::text,
 ALTER COLUMN dedup_key SET NOT NULL,
 ADD CONSTRAINT outbox_dedup_key_length CHECK (length(dedup_key) BETWEEN 1 AND 256),
 ADD CONSTRAINT outbox_destination_dedup UNIQUE(tenant_id,delivery_target,dedup_key),
 ADD CONSTRAINT outbox_lease_shape CHECK (
  (status='processing' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL AND attempts>0 AND delivered_at IS NULL)
  OR (status IN ('pending','failed') AND lease_owner IS NULL AND lease_until IS NULL AND delivered_at IS NULL)
  OR (status='delivered' AND lease_owner IS NULL AND lease_until IS NULL AND delivered_at IS NOT NULL));
CREATE INDEX outbox_due ON app.outbox_events(tenant_id,next_attempt_at,id) WHERE status IN ('pending','failed');
CREATE INDEX outbox_expired_lease ON app.outbox_events(tenant_id,lease_until,id) WHERE status='processing';
-- Producers cannot forge claims/acks or replace the event's delivery identity.
REVOKE INSERT,UPDATE ON app.outbox_events FROM app_runtime;
GRANT INSERT(tenant_id,id,event_type,payload,delivery_target,dedup_key,created_at)
 ON app.outbox_events TO app_runtime;

ALTER TABLE app.idempotency_records
 ADD COLUMN claim_token uuid NOT NULL DEFAULT uuidv7(),
 ADD COLUMN response_body_reference text CHECK (length(response_body_reference) BETWEEN 1 AND 2048),
 ADD COLUMN completed_at timestamptz;
ALTER TABLE app.idempotency_records DROP CONSTRAINT idempotency_records_state_check;
ALTER TABLE app.idempotency_records ADD CONSTRAINT idempotency_records_state_check CHECK (state IN ('in_progress','completed')),
 ADD CONSTRAINT idempotency_response_shape CHECK (
  (state='in_progress' AND response_status IS NULL AND response_body_reference IS NULL AND completed_at IS NULL AND response_body IS NULL)
  OR (state='completed' AND response_status BETWEEN 100 AND 599 AND response_status IS NOT NULL
      AND response_body_reference IS NOT NULL AND completed_at IS NOT NULL AND response_body IS NULL));
-- Explicit per-attempt UUID + no-op ON CONFLICT UPDATE returns the original row:
-- matching returned claim_token identifies ownership; compare request_hash FIRST
-- for 422, then state for 409/replay. Hash canonicalization belongs to the caller.
REVOKE INSERT ON app.idempotency_records FROM app_runtime;
GRANT INSERT(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token,created_at)
 ON app.idempotency_records TO app_runtime;

DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_outbox_worker') THEN CREATE ROLE app_outbox_worker; END IF;
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_reliability_executor') THEN CREATE ROLE app_reliability_executor; END IF;
 -- Do not silently accept pre-existing privileged identities.
 IF EXISTS(SELECT FROM pg_auth_members WHERE member IN ('app_outbox_worker'::regrole,'app_reliability_executor'::regrole)
           OR roleid='app_reliability_executor'::regrole) THEN
  RAISE EXCEPTION 'RELIABILITY_ROLE_MEMBERSHIP_UNSAFE';
 END IF;
END $$;
ALTER ROLE app_outbox_worker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
ALTER ROLE app_reliability_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_outbox_worker,app_reliability_executor;
GRANT SELECT ON app.outbox_events TO app_reliability_executor;
GRANT UPDATE(status,lease_owner,lease_until,attempts,next_attempt_at,last_error,delivered_at)
 ON app.outbox_events TO app_reliability_executor;
CREATE POLICY reliability_outbox ON app.outbox_events TO app_reliability_executor
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

CREATE FUNCTION app.guard_inbox_identity() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 -- Let the enum CHECK report malformed states before interpreting transitions.
 IF NEW.status NOT IN ('pending','processing','processed','failed') THEN RETURN NEW; END IF;
 IF ROW(NEW.tenant_id,NEW.id,NEW.provider,NEW.provider_account_id,NEW.environment,NEW.provider_event_id,NEW.payload_hash,NEW.payload_reference,NEW.created_at)
  IS DISTINCT FROM ROW(OLD.tenant_id,OLD.id,OLD.provider,OLD.provider_account_id,OLD.environment,OLD.provider_event_id,OLD.payload_hash,OLD.payload_reference,OLD.created_at)
 OR (OLD.status='processed' AND NEW IS DISTINCT FROM OLD)
 OR (NEW.status<>OLD.status AND NOT (
  (OLD.status IN ('pending','failed') AND NEW.status='processing')
  OR (OLD.status='processing' AND NEW.status IN ('processed','failed')))) THEN
  RAISE EXCEPTION 'INBOX_IDENTITY_OR_TRANSITION_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER inbox_identity BEFORE UPDATE ON app.webhook_inbox
 FOR EACH ROW EXECUTE FUNCTION app.guard_inbox_identity();
CREATE FUNCTION app.guard_idempotency_identity() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE reclaim boolean;
BEGIN
 -- Takeover of a crashed owner's claim: still in_progress, already expired, same
 -- request; only the claim token and a fresh expiry may change.
 reclaim := OLD.state='in_progress' AND NEW.state='in_progress' AND OLD.expires_at<=clock_timestamp()
   AND NEW.expires_at>clock_timestamp() AND NEW.claim_token IS DISTINCT FROM OLD.claim_token;
 IF ROW(NEW.tenant_id,NEW.actor_kind,NEW.actor_id,NEW.operation,NEW.idempotency_key,NEW.request_hash,NEW.created_at)
  IS DISTINCT FROM ROW(OLD.tenant_id,OLD.actor_kind,OLD.actor_id,OLD.operation,OLD.idempotency_key,OLD.request_hash,OLD.created_at)
 OR (OLD.state='completed' AND NEW IS DISTINCT FROM OLD)
 OR (ROW(NEW.claim_token,NEW.expires_at) IS DISTINCT FROM ROW(OLD.claim_token,OLD.expires_at) AND NOT reclaim)
 -- Completion is fenced: the caller proves it holds the CURRENT claim, so a stale
 -- owner whose claim was taken over cannot write a result.
 OR (OLD.state='in_progress' AND NEW.state<>'in_progress'
     AND nullif(current_setting('app.idempotency_claim',true),'') IS DISTINCT FROM OLD.claim_token::text) THEN
  RAISE EXCEPTION 'IDEMPOTENCY_IDENTITY_OR_RESULT_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER idempotency_identity BEFORE UPDATE ON app.idempotency_records
 FOR EACH ROW EXECUTE FUNCTION app.guard_idempotency_identity();
-- One atomic claim: insert, or return the existing row (409 in_progress / 422
-- different hash / replay completed), taking over only an expired claim of the
-- same request. Invoker rights: RLS and runtime column grants still apply.
CREATE FUNCTION app.idempotency_claim(p_tenant uuid,p_actor_kind text,p_actor_id uuid,p_operation text,
 p_key text,p_hash bytea,p_ttl interval,p_claim uuid) RETURNS app.idempotency_records
LANGUAGE sql SET search_path=pg_catalog,app,pg_temp AS $$
 INSERT INTO app.idempotency_records AS r(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token)
 VALUES(p_tenant,p_actor_kind,p_actor_id,p_operation,p_key,p_hash,clock_timestamp()+p_ttl,p_claim)
 ON CONFLICT(tenant_id,actor_kind,actor_id,operation,idempotency_key) DO UPDATE SET
  claim_token=CASE WHEN r.state='in_progress' AND r.expires_at<=clock_timestamp() AND r.request_hash=EXCLUDED.request_hash
                   THEN EXCLUDED.claim_token ELSE r.claim_token END,
  expires_at=CASE WHEN r.state='in_progress' AND r.expires_at<=clock_timestamp() AND r.request_hash=EXCLUDED.request_hash
                  THEN EXCLUDED.expires_at ELSE r.expires_at END
 RETURNING r.*;
$$;

CREATE FUNCTION app.claim_outbox(p_tenant uuid,p_worker uuid,p_limit integer,p_lease_seconds integer DEFAULT 60)
RETURNS SETOF app.outbox_events LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_SCOPE' USING ERRCODE='42501';
 END IF;
 IF p_worker IS NULL OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000
 OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 1 AND 3600 THEN
  RAISE EXCEPTION 'OUTBOX_CLAIM_ARGUMENT' USING ERRCODE='22023';
 END IF;
 RETURN QUERY WITH due AS MATERIALIZED (
  SELECT e.tenant_id,e.id FROM app.outbox_events e WHERE e.tenant_id=p_tenant
   AND ((e.status IN ('pending','failed') AND e.next_attempt_at<=clock_timestamp())
        OR (e.status='processing' AND e.lease_until<=clock_timestamp()))
  ORDER BY e.next_attempt_at,e.id LIMIT p_limit FOR UPDATE SKIP LOCKED
 ) UPDATE app.outbox_events e SET status='processing',lease_owner=p_worker,
  lease_until=clock_timestamp()+make_interval(secs=>p_lease_seconds),attempts=e.attempts+1,last_error=NULL
  FROM due WHERE e.tenant_id=due.tenant_id AND e.id=due.id RETURNING e.*;
END $$;
CREATE FUNCTION app.complete_outbox(p_tenant uuid,p_id uuid,p_worker uuid,p_attempt integer)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ DECLARE e app.outbox_events; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'OUTBOX_TENANT_SCOPE' USING ERRCODE='42501';
 END IF;
 SELECT * INTO e FROM app.outbox_events WHERE tenant_id=p_tenant AND id=p_id FOR UPDATE;
 -- Clock check follows the lock: lock waits and long transactions cannot extend a lease.
 IF NOT FOUND OR e.status<>'processing' OR e.lease_owner IS DISTINCT FROM p_worker
 OR e.attempts IS DISTINCT FROM p_attempt OR e.lease_until<=clock_timestamp() THEN RETURN false; END IF;
 UPDATE app.outbox_events SET status='delivered',lease_owner=NULL,lease_until=NULL,last_error=NULL,delivered_at=clock_timestamp()
  WHERE tenant_id=p_tenant AND id=p_id;
 RETURN true;
END $$;
CREATE FUNCTION app.fail_outbox(p_tenant uuid,p_id uuid,p_worker uuid,p_attempt integer,p_error text,p_retry_seconds integer DEFAULT 60)
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
 UPDATE app.outbox_events SET status='failed',lease_owner=NULL,lease_until=NULL,last_error=p_error,
  next_attempt_at=clock_timestamp()+make_interval(secs=>p_retry_seconds) WHERE tenant_id=p_tenant AND id=p_id;
 RETURN true;
END $$;
-- Dedicated unreachable owner, never a runtime membership or PUBLIC capability.
GRANT CREATE ON SCHEMA app TO app_reliability_executor;
ALTER FUNCTION app.guard_inbox_identity() OWNER TO app_reliability_executor;
ALTER FUNCTION app.guard_idempotency_identity() OWNER TO app_reliability_executor;
ALTER FUNCTION app.idempotency_claim(uuid,text,uuid,text,text,bytea,interval,uuid) OWNER TO app_reliability_executor;
REVOKE ALL ON FUNCTION app.idempotency_claim(uuid,text,uuid,text,text,bytea,interval,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.idempotency_claim(uuid,text,uuid,text,text,bytea,interval,uuid) TO app_runtime;
ALTER FUNCTION app.claim_outbox(uuid,uuid,integer,integer) OWNER TO app_reliability_executor;
ALTER FUNCTION app.complete_outbox(uuid,uuid,uuid,integer) OWNER TO app_reliability_executor;
ALTER FUNCTION app.fail_outbox(uuid,uuid,uuid,integer,text,integer) OWNER TO app_reliability_executor;
REVOKE CREATE ON SCHEMA app FROM app_reliability_executor;
REVOKE ALL ON FUNCTION app.guard_inbox_identity(),app.guard_idempotency_identity(),
 app.claim_outbox(uuid,uuid,integer,integer),app.complete_outbox(uuid,uuid,uuid,integer),
 app.fail_outbox(uuid,uuid,uuid,integer,text,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.claim_outbox(uuid,uuid,integer,integer),app.complete_outbox(uuid,uuid,uuid,integer),
 app.fail_outbox(uuid,uuid,uuid,integer,text,integer) TO app_outbox_worker;
