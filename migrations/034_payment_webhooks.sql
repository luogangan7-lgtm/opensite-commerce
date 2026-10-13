-- Responsibility: trusted Fake account resolution and durable normalized payment
-- facts; inbox identity is reused. Dependencies: reliability, checkout and orders.
-- Caller: signed Payments consumer; provisioning alone may bind provider accounts.
-- Invariants: immutable account/tenant binding, runtime cannot create identities,
-- no raw payload retention, deterministic stream lock before checkout/order locks.
CREATE TABLE app.payment_provider_accounts (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 provider text NOT NULL CHECK(provider='fake'),
 provider_account_id text NOT NULL CHECK(provider_account_id ~ '^[A-Za-z0-9_-]{1,128}$'),
 environment text NOT NULL CHECK(environment='test'),
 enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,provider,provider_account_id,environment),
 UNIQUE(provider,provider_account_id,environment),
 UNIQUE(tenant_id,provider,environment)
);
CREATE TABLE app.payment_events (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL,
 provider text NOT NULL DEFAULT 'fake' CHECK(provider='fake'),
 provider_account_id text NOT NULL,
 environment text NOT NULL DEFAULT 'test' CHECK(environment='test'),
 provider_ref text NOT NULL CHECK(length(provider_ref) BETWEEN 1 AND 200),
 event_key text NOT NULL CHECK(event_key ~ '^[a-f0-9]{64}$'),
 claimed_order_id uuid NOT NULL,
 order_version bigint NOT NULL CHECK(order_version>0),
 status text NOT NULL CHECK(status IN('captured','failed','canceled','partially_refunded','refunded')),
 amount_minor bigint NOT NULL CHECK(amount_minor>0),
 currency char(3) NOT NULL CHECK(currency::text ~ '^[A-Z]{3}$'),
 applied boolean NOT NULL DEFAULT false,
 error_code text CHECK(error_code='state_conflict'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,event_key),
 FOREIGN KEY(tenant_id,id) REFERENCES app.webhook_inbox(tenant_id,id),
 FOREIGN KEY(tenant_id,provider,provider_account_id,environment) REFERENCES app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment),
 CHECK(NOT applied OR error_code IS NULL)
);
CREATE INDEX payment_events_pending ON app.payment_events(tenant_id,provider_account_id,provider_ref,created_at,id) WHERE NOT applied;
ALTER TABLE app.payment_provider_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_provider_accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE app.payment_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_provider_accounts USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY tenant_scope ON app.payment_events USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.payment_provider_accounts,app.payment_events FROM PUBLIC,app_runtime;
GRANT SELECT ON app.payment_events TO app_runtime;
GRANT INSERT(tenant_id,id,provider_account_id,provider_ref,event_key,claimed_order_id,order_version,status,amount_minor,currency) ON app.payment_events TO app_runtime;
GRANT UPDATE(applied,error_code) ON app.payment_events TO app_runtime;

DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_payment_resolver') THEN
  CREATE ROLE app_payment_resolver NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB NOREPLICATION NOINHERIT;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_payment_resolver' AND
  (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_payment_resolver'::regrole OR roleid='app_payment_resolver'::regrole)
 OR has_schema_privilege('app_payment_resolver','app','CREATE') THEN
  RAISE EXCEPTION 'PAYMENT_RESOLVER_UNSAFE';
 END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_payment_resolver;
GRANT SELECT,UPDATE(enabled) ON app.payment_provider_accounts TO app_payment_resolver;
CREATE POLICY payment_resolver ON app.payment_provider_accounts TO app_payment_resolver USING(true);
CREATE FUNCTION app.resolve_fake_payment_account(p_account text) RETURNS TABLE(tenant_id uuid,at_time timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT a.tenant_id,clock_timestamp() FROM app.payment_provider_accounts a
 WHERE a.provider='fake' AND a.environment='test' AND a.provider_account_id=p_account AND a.enabled FOR SHARE OF a
$$;
ALTER FUNCTION app.resolve_fake_payment_account(text) OWNER TO app_payment_resolver;
REVOKE ALL ON FUNCTION app.resolve_fake_payment_account(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.resolve_fake_payment_account(text) TO app_runtime;

CREATE FUNCTION app.lock_payment_stream(p_account text,p_ref text) RETURNS timestamptz LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('payments:fake:test:'||p_account||':'||p_ref,19));
 RETURN clock_timestamp();
END $$;
REVOKE ALL ON FUNCTION app.lock_payment_stream(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.lock_payment_stream(text,text) TO app_runtime;

-- Reliability owns inbox writes: one primitive binds duplicate identity to the
-- exact payload hash. Stream locking is always taken first by its caller.
CREATE FUNCTION app.admit_payment_inbox(p_account text,p_event text,p_hash bytea) RETURNS uuid
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid := nullif(current_setting('app.tenant_id',true),'')::uuid; r app.webhook_inbox; new_id uuid := uuidv7();
BEGIN
 INSERT INTO app.webhook_inbox(tenant_id,id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(t,new_id,'fake',p_account,'test',p_event,p_hash,'payment-event:'||new_id::text)
 ON CONFLICT(tenant_id,provider,provider_account_id,environment,provider_event_id) DO NOTHING;
 SELECT * INTO STRICT r FROM app.webhook_inbox WHERE tenant_id=t AND provider='fake' AND provider_account_id=p_account AND environment='test' AND provider_event_id=p_event FOR UPDATE;
 IF r.payload_hash IS DISTINCT FROM p_hash THEN RAISE EXCEPTION 'payment event content conflict' USING ERRCODE='23505'; END IF;
 RETURN r.id;
END $$;
REVOKE ALL ON FUNCTION app.admit_payment_inbox(text,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.admit_payment_inbox(text,text,bytea) TO app_runtime;
CREATE FUNCTION app.finish_payment_inbox(p_id uuid,p_success boolean) RETURNS void LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid := nullif(current_setting('app.tenant_id',true),'')::uuid;
BEGIN
 UPDATE app.webhook_inbox SET status='processing',attempts=attempts+1 WHERE tenant_id=t AND id=p_id AND status IN('pending','failed');
 UPDATE app.webhook_inbox SET status=CASE WHEN p_success THEN 'processed' ELSE 'failed' END,
 processed_at=CASE WHEN p_success THEN clock_timestamp() ELSE NULL END WHERE tenant_id=t AND id=p_id AND status='processing';
END $$;
REVOKE ALL ON FUNCTION app.finish_payment_inbox(uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.finish_payment_inbox(uuid,boolean) TO app_runtime;

CREATE FUNCTION app.guard_payment_fact() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'payment facts are permanent' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='payment_provider_accounts' THEN
  IF (to_jsonb(NEW)-'enabled') IS DISTINCT FROM (to_jsonb(OLD)-'enabled') THEN RAISE EXCEPTION 'payment account binding immutable' USING ERRCODE='23514'; END IF;
 ELSIF TG_OP='INSERT' THEN
  IF NEW.applied OR NEW.error_code IS NOT NULL THEN RAISE EXCEPTION 'payment fact starts pending' USING ERRCODE='23514'; END IF;
 ELSIF (to_jsonb(NEW)-ARRAY['applied','error_code']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['applied','error_code'])
 OR (OLD.applied AND NEW IS DISTINCT FROM OLD) THEN
  RAISE EXCEPTION 'payment fact immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.guard_payment_fact() FROM PUBLIC;
CREATE TRIGGER payment_account_guard BEFORE UPDATE OR DELETE ON app.payment_provider_accounts FOR EACH ROW EXECUTE FUNCTION app.guard_payment_fact();
CREATE TRIGGER payment_event_guard BEFORE INSERT OR UPDATE OR DELETE ON app.payment_events FOR EACH ROW EXECUTE FUNCTION app.guard_payment_fact();

ALTER TABLE app.checkout_sessions ADD COLUMN refunded_net_minor bigint NOT NULL DEFAULT 0 CHECK(refunded_net_minor BETWEEN 0 AND items_net_minor),
 ADD COLUMN refunded_tax_minor bigint NOT NULL DEFAULT 0 CHECK(refunded_tax_minor BETWEEN 0 AND tax_minor),
 ADD COLUMN refunded_shipping_minor bigint NOT NULL DEFAULT 0 CHECK(refunded_shipping_minor BETWEEN 0 AND shipping_minor);
GRANT UPDATE(refunded_net_minor,refunded_tax_minor,refunded_shipping_minor) ON app.checkout_sessions TO app_runtime;
CREATE FUNCTION app.guard_checkout_refund() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.refunded_net_minor<>0 OR NEW.refunded_tax_minor<>0 OR NEW.refunded_shipping_minor<>0 THEN RAISE EXCEPTION 'refund starts zero' USING ERRCODE='23514'; END IF;
 ELSIF NEW.refunded_net_minor<OLD.refunded_net_minor OR NEW.refunded_tax_minor<OLD.refunded_tax_minor OR NEW.refunded_shipping_minor<OLD.refunded_shipping_minor
 OR ((NEW.refunded_net_minor,NEW.refunded_tax_minor,NEW.refunded_shipping_minor) IS DISTINCT FROM (OLD.refunded_net_minor,OLD.refunded_tax_minor,OLD.refunded_shipping_minor) AND NEW.phase<>'paid') THEN
  RAISE EXCEPTION 'refund counters are monotonic after capture' USING ERRCODE='23514';
 END IF; RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.guard_checkout_refund() FROM PUBLIC;
CREATE TRIGGER checkout_refund_guard BEFORE INSERT OR UPDATE ON app.checkout_sessions FOR EACH ROW EXECUTE FUNCTION app.guard_checkout_refund();
