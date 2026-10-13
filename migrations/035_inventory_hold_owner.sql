-- Responsibility: DB-enforced holder possession for non-order inventory holds.
-- Depends on inventory/order/queue capabilities; used by controlled inventory commands.
-- Invariants: readable owner/id/hash is not a bearer; only due queue cleanup bypasses it.
-- Legacy non-order rows have no retrievable bearer and can only expire. Order
-- completion remains within the existing exact owner/SKU/quantity Orders primitive.
ALTER TABLE app.inventory_reservations ADD COLUMN holder_hash bytea
 CHECK(holder_hash IS NULL OR octet_length(holder_hash)=32);

CREATE FUNCTION app.inventory_reserve_owned(p_sku uuid,p_quantity bigint,p_owner_type text,p_owner_id uuid,p_key text,p_ttl_seconds integer,p_holder_hash bytea)
RETURNS app.inventory_reservations LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); w uuid; r app.inventory_reservations; s app.stock_levels; h bytea;
BEGIN
 IF p_sku IS NULL OR p_quantity IS NULL OR p_quantity<=0 OR p_owner_id IS NULL
  OR p_owner_type IS NULL OR btrim(p_owner_type)='' OR length(p_owner_type)>64
  OR (p_holder_hash IS NOT NULL AND octet_length(p_holder_hash)<>32)
  OR (p_holder_hash IS NULL AND p_owner_type<>'order')
  OR p_key IS NULL OR btrim(p_key)='' OR length(p_key)>128 OR p_ttl_seconds IS NULL OR p_ttl_seconds<=0 THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 h:=sha256(convert_to(jsonb_build_object('sku',p_sku,'quantity',p_quantity,'owner_type',p_owner_type,
  'owner_id',p_owner_id,'ttl_seconds',p_ttl_seconds)::text,'UTF8'));
 -- Preserve historical order hashes; non-order idempotency also proves bearer possession.
 -- Serialize the key BEFORE stock; replay never locks an already-existing reservation.
 PERFORM pg_advisory_xact_lock(hashtextextended(t::text||':'||p_key,0));
 SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND idempotency_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'INVENTORY_IDEMPOTENCY_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r;
 END IF;
 SELECT id INTO w FROM app.warehouses WHERE tenant_id=t;
 SELECT * INTO s FROM app.stock_levels WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'INVENTORY_STOCK_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF s.on_hand-s.reserved<p_quantity THEN
  RAISE EXCEPTION 'INVENTORY_INSUFFICIENT_STOCK' USING ERRCODE='P0001';
 END IF;
 UPDATE app.stock_levels SET reserved=reserved+p_quantity WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku RETURNING * INTO s;
 INSERT INTO app.inventory_reservations(tenant_id,warehouse_id,sku_id,quantity,owner_type,owner_id,idempotency_key,request_hash,ttl_seconds,expires_at,holder_hash)
 VALUES(t,w,p_sku,p_quantity,p_owner_type,p_owner_id,p_key,h,p_ttl_seconds,clock_timestamp()+make_interval(secs=>p_ttl_seconds),p_holder_hash) RETURNING * INTO r;
 INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,reservation_id,owner_type,owner_id,kind,quantity,
  on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
 VALUES(t,w,p_sku,r.id,p_owner_type,p_owner_id,'reserve',p_quantity,0,p_quantity,s.on_hand,s.reserved,'reservation created');
 RETURN r;
END $fn$;

-- This historical signature remains solely for the Orders reserve contract.
-- It cannot create/replay a non-order hold without proof, including case aliases.
CREATE OR REPLACE FUNCTION app.inventory_reserve(p_sku uuid,p_quantity bigint,p_owner_type text,p_owner_id uuid,p_key text,p_ttl_seconds integer DEFAULT 900)
RETURNS app.inventory_reservations LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF p_owner_type IS DISTINCT FROM 'order' THEN
  RAISE EXCEPTION 'INVENTORY_HOLDER_REQUIRED' USING ERRCODE='42501';
 END IF;
 RETURN app.inventory_reserve_owned(p_sku,p_quantity,p_owner_type,p_owner_id,p_key,p_ttl_seconds,NULL);
END $fn$;

-- Caller generates 32 bytes with a CSPRNG and retains them privately. Database
-- rows, receipts, movement reasons and outbox never store/return the plaintext.
CREATE FUNCTION app.inventory_reserve_non_order(p_sku uuid,p_quantity bigint,p_owner_type text,p_owner_id uuid,p_key text,p_ttl_seconds integer,p_bearer bytea)
RETURNS app.inventory_reservations LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF p_bearer IS NULL OR octet_length(p_bearer)<>32 THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 IF lower(p_owner_type)='order' THEN
  RAISE EXCEPTION 'INVENTORY_ORDER_OWNED_RESERVATION' USING ERRCODE='42501';
 END IF;
 RETURN app.inventory_reserve_owned(p_sku,p_quantity,p_owner_type,p_owner_id,p_key,p_ttl_seconds,sha256(p_bearer));
END $fn$;

DROP FUNCTION app.inventory_release_non_order(uuid,text,uuid);
DROP FUNCTION app.inventory_commit_non_order(uuid,text,uuid);
DROP FUNCTION app.inventory_finish_non_order(uuid,text,text,uuid);
CREATE FUNCTION app.inventory_finish_non_order(p_id uuid,p_target text,p_owner_type text,p_owner_id uuid,p_bearer bytea)
RETURNS app.inventory_reservations LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); r app.inventory_reservations;
BEGIN
 IF p_id IS NULL OR p_owner_type IS NULL OR btrim(p_owner_type)='' OR length(p_owner_type)>64 OR p_owner_id IS NULL
  OR p_bearer IS NULL OR octet_length(p_bearer)<>32 THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'INVENTORY_RESERVATION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF lower(r.owner_type)='order' THEN
  RAISE EXCEPTION 'INVENTORY_ORDER_OWNED_RESERVATION' USING ERRCODE='42501';
 END IF;
 IF r.owner_type<>p_owner_type OR r.owner_id<>p_owner_id THEN
  RAISE EXCEPTION 'INVENTORY_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501';
 END IF;
 -- Authenticate before any terminal replay: a finished row never becomes public.
 IF r.holder_hash IS NULL OR r.holder_hash<>sha256(p_bearer) THEN
  RAISE EXCEPTION 'INVENTORY_HOLDER_MISMATCH' USING ERRCODE='42501';
 END IF;
 RETURN app.inventory_finish(p_id,p_target);
END $fn$;
CREATE FUNCTION app.inventory_release_non_order(p_id uuid,p_owner_type text,p_owner_id uuid,p_bearer bytea)
RETURNS app.inventory_reservations LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish_non_order(p_id,''released'',p_owner_type,p_owner_id,p_bearer)';
CREATE FUNCTION app.inventory_commit_non_order(p_id uuid,p_owner_type text,p_owner_id uuid,p_bearer bytea)
RETURNS app.inventory_reservations LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish_non_order(p_id,''committed'',p_owner_type,p_owner_id,p_bearer)';

GRANT CREATE ON SCHEMA app TO app_inventory_executor;
ALTER FUNCTION app.inventory_reserve_owned(uuid,bigint,text,uuid,text,integer,bytea) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_reserve_non_order(uuid,bigint,text,uuid,text,integer,bytea) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_finish_non_order(uuid,text,text,uuid,bytea) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_release_non_order(uuid,text,uuid,bytea) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_commit_non_order(uuid,text,uuid,bytea) OWNER TO app_inventory_executor;
REVOKE CREATE ON SCHEMA app FROM app_inventory_executor;
REVOKE ALL ON FUNCTION app.inventory_reserve_owned(uuid,bigint,text,uuid,text,integer,bytea),
 app.inventory_reserve_non_order(uuid,bigint,text,uuid,text,integer,bytea),
 app.inventory_finish_non_order(uuid,text,text,uuid,bytea),app.inventory_release_non_order(uuid,text,uuid,bytea),
 app.inventory_commit_non_order(uuid,text,uuid,bytea)
 FROM PUBLIC,app_runtime,app_inventory_manager,app_orders_executor,app_queue_worker;
GRANT EXECUTE ON FUNCTION app.inventory_reserve_non_order(uuid,bigint,text,uuid,text,integer,bytea),
 app.inventory_release_non_order(uuid,text,uuid,bytea),app.inventory_commit_non_order(uuid,text,uuid,bytea) TO app_runtime;
-- Automatic expiry is a separate system authority, not an actor's runtime token.
-- Its existing primitive accepts only batch size, only due rows, and DB wall clock.
REVOKE EXECUTE ON FUNCTION app.inventory_expire_due(integer) FROM PUBLIC,app_runtime,app_inventory_manager,app_orders_executor;
GRANT EXECUTE ON FUNCTION app.inventory_expire_due(integer) TO app_queue_worker;
