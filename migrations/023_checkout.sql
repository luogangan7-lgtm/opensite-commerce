-- Responsibility: checkout's durable intent, permanent request/event receipts and
-- provider-operation fence. Depends on immutable Orders, tenant RLS and Outbox.
-- Called by authorized checkout transactions; never performs provider IO.
-- Invariants: original payable is immutable; confirm/cancel compete BEFORE IO;
-- tenant-composite FKs, permanent actor/key binding, append-only event identity.
CREATE TABLE app.checkout_sessions (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), id uuid NOT NULL,
 actor_kind text NOT NULL CHECK(actor_kind IN('guest','customer')),
 actor_id text NOT NULL CHECK((actor_kind='guest' AND actor_id ~ '^[a-f0-9]{64}$') OR
  (actor_kind='customer' AND actor_id ~ '^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$' AND actor_id<>'00000000-0000-0000-0000-000000000000')),
 request_key text NOT NULL CHECK(length(request_key) BETWEEN 1 AND 128 AND request_key ~ '^[!-~]+$'),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 cart_id uuid NOT NULL, cart_version bigint NOT NULL CHECK(cart_version>0),
 order_id uuid NOT NULL,
 address_snapshot jsonb NOT NULL CHECK(jsonb_typeof(address_snapshot)='object'),
 currency char(3) NOT NULL CHECK(currency::text ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK(amount_minor>0),
 items_net_minor bigint NOT NULL CHECK(items_net_minor>=0),
 tax_minor bigint NOT NULL CHECK(tax_minor>=0),
 shipping_minor bigint NOT NULL CHECK(shipping_minor>=0),
 payable_version bigint NOT NULL CHECK(payable_version>0),
 phase text NOT NULL DEFAULT 'pending' CHECK(phase IN('pending','ready','confirming','failing','cancelling','paid','cancelled','failed')),
 provider_ref text NOT NULL DEFAULT '' CHECK(length(provider_ref)<=512),
 start_result jsonb, final_result jsonb,
 claim_kind text, claim_key text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,actor_kind,actor_id,request_key), UNIQUE(tenant_id,order_id),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 CHECK(amount_minor=items_net_minor+tax_minor+shipping_minor),
 CHECK(start_result IS NULL OR jsonb_typeof(start_result)='object'),
 CHECK(final_result IS NULL OR jsonb_typeof(final_result)='object'),
 CHECK((phase='pending' AND provider_ref='' AND start_result IS NULL) OR
  (phase='failed') OR (phase<>'pending' AND provider_ref<>'' AND start_result IS NOT NULL)),
 CHECK((phase IN('pending','ready') AND claim_kind IS NULL AND claim_key IS NULL) OR
  (phase NOT IN('pending','ready') AND claim_kind IN('payment','cancel','create_failure') AND claim_key IS NOT NULL))
);
CREATE TABLE app.checkout_attempts (
 tenant_id uuid NOT NULL, event_kind text NOT NULL CHECK(event_kind IN('payment','cancel')),
 event_key text NOT NULL CHECK(length(event_key) BETWEEN 1 AND 256),
 checkout_id uuid NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 result jsonb CHECK(result IS NULL OR jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,event_kind,event_key),
 FOREIGN KEY(tenant_id,checkout_id) REFERENCES app.checkout_sessions(tenant_id,id)
);
ALTER TABLE app.checkout_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.checkout_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.checkout_sessions
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
ALTER TABLE app.checkout_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.checkout_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.checkout_attempts
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.checkout_sessions,app.checkout_attempts FROM PUBLIC;
GRANT SELECT,INSERT ON app.checkout_sessions,app.checkout_attempts TO app_runtime;
GRANT UPDATE(phase,provider_ref,start_result,final_result,claim_kind,claim_key,updated_at) ON app.checkout_sessions TO app_runtime;
GRANT UPDATE(result) ON app.checkout_attempts TO app_runtime;

CREATE FUNCTION app.checkout_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE o app.orders;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'checkout receipt is permanent' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' THEN
  IF TG_TABLE_NAME='checkout_attempts' THEN
   IF NEW.result IS NOT NULL THEN RAISE EXCEPTION 'checkout event starts incomplete' USING ERRCODE='23514'; END IF;
   RETURN NEW;
  END IF;
  SELECT * INTO o FROM app.orders WHERE tenant_id=NEW.tenant_id AND id=NEW.order_id;
  IF NOT FOUND OR NOT o.sealed OR o.state<>'pending_payment' OR
   (NEW.currency,NEW.amount_minor,NEW.items_net_minor,NEW.tax_minor,NEW.shipping_minor,NEW.payable_version)
     IS DISTINCT FROM (o.currency,o.total_minor,o.items_net_minor,o.tax_minor,o.shipping_minor,o.version)
   OR (NEW.actor_kind='customer' AND NEW.actor_id IS DISTINCT FROM o.customer_id::text)
   OR (NEW.actor_kind='guest' AND o.customer_id IS NOT NULL)
   OR NEW.phase<>'pending' OR NEW.provider_ref<>'' OR NEW.start_result IS NOT NULL OR NEW.final_result IS NOT NULL
   OR NEW.claim_kind IS NOT NULL OR NEW.claim_key IS NOT NULL THEN
   RAISE EXCEPTION 'checkout admission snapshot mismatch' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF TG_TABLE_NAME='checkout_attempts' THEN
  IF (NEW.tenant_id,NEW.event_kind,NEW.event_key,NEW.checkout_id,NEW.request_hash,NEW.created_at)
    IS DISTINCT FROM (OLD.tenant_id,OLD.event_kind,OLD.event_key,OLD.checkout_id,OLD.request_hash,OLD.created_at)
    OR (OLD.result IS NOT NULL AND NEW.result IS DISTINCT FROM OLD.result) THEN
   RAISE EXCEPTION 'checkout event receipt immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF (NEW.tenant_id,NEW.id,NEW.actor_kind,NEW.actor_id,NEW.request_key,NEW.request_hash,NEW.cart_id,NEW.cart_version,
     NEW.order_id,NEW.address_snapshot,NEW.currency,NEW.amount_minor,NEW.items_net_minor,NEW.tax_minor,NEW.shipping_minor,NEW.payable_version,NEW.created_at)
 IS DISTINCT FROM
    (OLD.tenant_id,OLD.id,OLD.actor_kind,OLD.actor_id,OLD.request_key,OLD.request_hash,OLD.cart_id,OLD.cart_version,
     OLD.order_id,OLD.address_snapshot,OLD.currency,OLD.amount_minor,OLD.items_net_minor,OLD.tax_minor,OLD.shipping_minor,OLD.payable_version,OLD.created_at)
 OR (OLD.provider_ref<>'' AND NEW.provider_ref IS DISTINCT FROM OLD.provider_ref)
 OR (OLD.start_result IS NOT NULL AND NEW.start_result IS DISTINCT FROM OLD.start_result)
 OR (OLD.final_result IS NOT NULL AND NEW.final_result IS DISTINCT FROM OLD.final_result)
 OR (OLD.claim_key IS NOT NULL AND (NEW.claim_key,NEW.claim_kind) IS DISTINCT FROM (OLD.claim_key,OLD.claim_kind)) THEN
  RAISE EXCEPTION 'checkout snapshot immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.phase<>OLD.phase AND NOT
   ((OLD.phase='pending' AND NEW.phase IN('ready','failed')) OR
    (OLD.phase='ready' AND NEW.phase IN('confirming','cancelling')) OR
    (OLD.phase='confirming' AND NEW.phase IN('paid','failing')) OR
    (OLD.phase='failing' AND NEW.phase='failed') OR
    (OLD.phase='cancelling' AND NEW.phase IN('cancelled','failed'))) THEN
  RAISE EXCEPTION 'checkout terminal/fence transition denied' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.checkout_guard() FROM PUBLIC;
CREATE TRIGGER checkout_sessions_guard BEFORE INSERT OR UPDATE OR DELETE ON app.checkout_sessions
 FOR EACH ROW EXECUTE FUNCTION app.checkout_guard();
CREATE TRIGGER checkout_attempts_guard BEFORE INSERT OR UPDATE OR DELETE ON app.checkout_attempts
 FOR EACH ROW EXECUTE FUNCTION app.checkout_guard();
