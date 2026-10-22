-- stable-id: c18b-checkout-source
-- depends-on: c18b-quote-conversion
-- Responsibility: checkout source admission and the existing Orders lock fence.
-- Dependencies: checkout032, sealed Orders020 and the quote conversion proposal.
-- Callers: checkout Start/Adopt/Confirm and Orders Create/Transition.
-- Invariants: exact owner/payable, immutable source, session -> order -> sorted
-- reservations -> stock; provider IO never runs here. No new authority surface.
ALTER TABLE app.checkout_sessions
 ADD COLUMN source_kind text NOT NULL DEFAULT 'cart',
 ALTER COLUMN cart_id DROP NOT NULL,
 ALTER COLUMN cart_version DROP NOT NULL,
 ADD CONSTRAINT checkout_source CHECK(
  (source_kind='cart' AND cart_id IS NOT NULL AND cart_version IS NOT NULL AND cart_version>0)
  OR (source_kind='existing_order' AND actor_kind='customer' AND cart_id IS NULL AND cart_version IS NULL));

-- Retain the original exact Orders-owned admission checks and receipt phases.
-- A session source cannot be rewritten even by an administrative table writer.
CREATE OR REPLACE FUNCTION app.checkout_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE o app.orders;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'checkout receipt is permanent' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' THEN
  IF TG_TABLE_NAME='checkout_attempts' THEN
   IF NEW.result IS NOT NULL THEN RAISE EXCEPTION 'checkout event starts incomplete' USING ERRCODE='23514'; END IF;
   RETURN NEW;
  END IF;
  -- Runtime has SELECT but no UPDATE on Orders: delegate the parent row lock
  -- to the same narrowly granted Orders fence, with no hold rows requested.
  PERFORM app.orders_lock_reservations(NEW.order_id,ARRAY[]::uuid[]);
  SELECT * INTO o FROM app.orders WHERE tenant_id=NEW.tenant_id AND id=NEW.order_id;
  IF NOT FOUND OR NOT o.sealed OR o.state<>'pending_payment' OR
   (NEW.currency,NEW.amount_minor,NEW.items_net_minor,NEW.tax_minor,NEW.shipping_minor,NEW.payable_version)
     IS DISTINCT FROM (o.currency,o.total_minor,o.items_net_minor,o.tax_minor,o.shipping_minor,o.version)
   OR (NEW.actor_kind='customer' AND NEW.actor_id IS DISTINCT FROM o.customer_id::text)
   OR (NEW.actor_kind='guest' AND o.customer_id IS NOT NULL)
   OR (NEW.source_kind='existing_order' AND (NEW.actor_kind<>'customer' OR NEW.cart_id IS NOT NULL OR NEW.cart_version IS NOT NULL))
   OR (NEW.source_kind='cart' AND (NEW.cart_id IS NULL OR NEW.cart_version IS NULL OR NEW.cart_version<=0))
   OR NEW.source_kind NOT IN('cart','existing_order')
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
 IF (NEW.tenant_id,NEW.id,NEW.actor_kind,NEW.actor_id,NEW.request_key,NEW.request_hash,NEW.source_kind,NEW.cart_id,NEW.cart_version,
     NEW.order_id,NEW.address_snapshot,NEW.currency,NEW.amount_minor,NEW.items_net_minor,NEW.tax_minor,NEW.shipping_minor,NEW.payable_version,NEW.created_at)
 IS DISTINCT FROM
    (OLD.tenant_id,OLD.id,OLD.actor_kind,OLD.actor_id,OLD.request_key,OLD.request_hash,OLD.source_kind,OLD.cart_id,OLD.cart_version,
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

-- Strengthen the SAME #49 public fence instead of inventing a second holder
-- authority. Create prelocks explicit holds before inserting its parent, so an
-- absent order is permitted; existing orders always lock before their holds.
CREATE OR REPLACE FUNCTION app.orders_lock_reservations(p_id uuid,p_reservations uuid[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.orders_context(); n integer;
BEGIN
 IF p_id IS NULL OR p_reservations IS NULL OR cardinality(p_reservations)>1000
  OR array_position(p_reservations,NULL) IS NOT NULL THEN
  RAISE EXCEPTION 'ORDER_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 PERFORM id FROM app.orders WHERE tenant_id=t AND id=p_id FOR UPDATE;
 WITH locked AS (SELECT id FROM app.inventory_reservations
  WHERE tenant_id=t AND id=ANY(p_reservations) AND owner_type='order' AND owner_id=p_id
  ORDER BY id FOR UPDATE)
 SELECT count(*) INTO n FROM locked;
 IF n<>(SELECT count(DISTINCT x) FROM unnest(p_reservations) x) THEN
  RAISE EXCEPTION 'ORDER_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501';
 END IF;
END $fn$;
