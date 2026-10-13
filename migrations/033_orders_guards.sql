-- Responsibility: orders creation replay before stock locks; owner-bound inventory terminal commands.
-- Owner: orders. Depends on 020_orders; used by orders service and trusted checkout.
-- Invariants: existing snapshots remain sealed; no legacy tenant-only terminal capability.
-- Compatibility: old unbound calls fail closed. Roll back code only after adapting callers;
-- database rollback requires a forward migration, never regrant the unbound runtime path.
CREATE FUNCTION app.orders_check_creation(p_id uuid,p_key text,p_hash bytea) RETURNS SETOF app.orders
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.orders_context(); o app.orders;
BEGIN
 IF p_id IS NULL OR p_key IS NULL OR btrim(p_key)='' OR length(p_key)>128 OR p_hash IS NULL OR octet_length(p_hash)<>32 THEN
  RAISE EXCEPTION 'ORDER_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 -- Key-scoped lock: same-key retries serialize through reservation and sealing, while
 -- unrelated orders keep reserving concurrently. The tenant-wide number lock stays inside
 -- orders_create, taken after stock locks as in explicit-reservation callers (one lock order).
 PERFORM pg_advisory_xact_lock(hashtextextended('orders-key:'||t::text||':'||p_key,0));
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF o.id<>p_id OR o.request_hash<>p_hash THEN
   RAISE EXCEPTION 'ORDER_IDEMPOTENCY_KEY_REUSED' USING ERRCODE='22023';
  END IF;
  RETURN NEXT o;
 END IF;
END $fn$;
GRANT CREATE ON SCHEMA app TO app_orders_executor;
ALTER FUNCTION app.orders_check_creation(uuid,text,bytea) OWNER TO app_orders_executor;
REVOKE CREATE ON SCHEMA app FROM app_orders_executor;
REVOKE ALL ON FUNCTION app.orders_check_creation(uuid,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.orders_check_creation(uuid,text,bytea) TO app_runtime;

-- Drop only superseded command signatures; the private terminal algorithm stays unchanged.
DROP FUNCTION app.inventory_release_non_order(uuid);
DROP FUNCTION app.inventory_commit_non_order(uuid);
DROP FUNCTION app.inventory_finish_non_order(uuid,text);
CREATE FUNCTION app.inventory_finish_non_order(p_id uuid,p_target text,p_owner_type text,p_owner_id uuid) RETURNS app.inventory_reservations
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); r app.inventory_reservations;
BEGIN
 IF p_id IS NULL OR p_owner_type IS NULL OR btrim(p_owner_type)='' OR length(p_owner_type)>64 OR p_owner_id IS NULL THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'INVENTORY_RESERVATION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 -- Case variants cannot evade the order-only terminal capability boundary.
 IF lower(r.owner_type)='order' THEN
  RAISE EXCEPTION 'INVENTORY_ORDER_OWNED_RESERVATION' USING ERRCODE='42501';
 END IF;
 IF r.owner_type<>p_owner_type OR r.owner_id<>p_owner_id THEN
  RAISE EXCEPTION 'INVENTORY_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501';
 END IF;
 RETURN app.inventory_finish(p_id,p_target);
END $fn$;
CREATE FUNCTION app.inventory_release_non_order(p_id uuid,p_owner_type text,p_owner_id uuid) RETURNS app.inventory_reservations
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish_non_order(p_id,''released'',p_owner_type,p_owner_id)';
CREATE FUNCTION app.inventory_commit_non_order(p_id uuid,p_owner_type text,p_owner_id uuid) RETURNS app.inventory_reservations
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish_non_order(p_id,''committed'',p_owner_type,p_owner_id)';
GRANT CREATE ON SCHEMA app TO app_inventory_executor;
ALTER FUNCTION app.inventory_finish_non_order(uuid,text,text,uuid) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_release_non_order(uuid,text,uuid) OWNER TO app_inventory_executor;
ALTER FUNCTION app.inventory_commit_non_order(uuid,text,uuid) OWNER TO app_inventory_executor;
REVOKE CREATE ON SCHEMA app FROM app_inventory_executor;
REVOKE ALL ON FUNCTION app.inventory_finish_non_order(uuid,text,text,uuid),app.inventory_release_non_order(uuid,text,uuid),app.inventory_commit_non_order(uuid,text,uuid) FROM PUBLIC,app_runtime,app_inventory_manager,app_orders_executor;
GRANT EXECUTE ON FUNCTION app.inventory_release_non_order(uuid,text,uuid),app.inventory_commit_non_order(uuid,text,uuid) TO app_runtime;

CREATE OR REPLACE FUNCTION app.orders_create(p_id uuid,p_key text,p_hash bytea,p_customer uuid,p_guest jsonb,
 p_currency text,p_shipping bigint,p_shipping_snapshot jsonb,p_lines jsonb,p_adjustments jsonb DEFAULT '[]')
RETURNS app.orders LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.orders_context(); o app.orders; l record; a record; r app.inventory_reservations;
 net bigint:=0; tax bigint:=0; adj bigint:=0; number bigint; snapshot_total bigint;
BEGIN
 IF p_id IS NULL OR p_key IS NULL OR btrim(p_key)='' OR length(p_key)>128
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_currency IS NULL OR p_currency !~ '^[A-Z]{3}$'
  OR p_shipping IS NULL OR p_shipping<0 OR p_shipping_snapshot IS NULL OR jsonb_typeof(p_shipping_snapshot)<>'object'
  OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR jsonb_array_length(p_lines) NOT BETWEEN 1 AND 1000
  OR p_adjustments IS NULL OR jsonb_typeof(p_adjustments)<>'array' OR jsonb_array_length(p_adjustments)>1000
  OR NOT ((p_customer IS NOT NULL AND p_guest IS NULL) OR
   (p_customer IS NULL AND p_guest IS NOT NULL AND jsonb_typeof(p_guest)='object'
    AND nullif(btrim(p_guest->>'email'),'') IS NOT NULL AND length(p_guest->>'email')<=320)) THEN
  RAISE EXCEPTION 'ORDER_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 -- Tenant lock allocates monotonic order numbers and serializes creation-key lookup.
 -- In RR/SERIALIZABLE a conflicting snapshot can fail 40001/23505; caller retries TX.
 PERFORM pg_advisory_xact_lock(hashtextextended('orders-number:'||t::text,0));
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF o.id<>p_id OR o.request_hash<>p_hash THEN
   RAISE EXCEPTION 'ORDER_IDEMPOTENCY_KEY_REUSED' USING ERRCODE='22023';
  END IF;
  RETURN o;
 END IF;
 FOR l IN SELECT * FROM jsonb_to_recordset(p_lines) AS x(id uuid,sku_id uuid,sku text,quantity bigint,
  unit_price_minor bigint,line_net_minor bigint,line_tax_minor bigint,line_total_minor bigint,reservation_id uuid,tax_snapshot jsonb) ORDER BY reservation_id LOOP
  IF l.id IS NULL OR l.sku_id IS NULL OR l.sku IS NULL OR btrim(l.sku)='' OR length(l.sku)>255
   OR l.quantity IS NULL OR l.quantity<=0 OR l.unit_price_minor IS NULL OR l.unit_price_minor<0
   OR l.line_net_minor IS NULL OR l.line_net_minor<0 OR l.line_tax_minor IS NULL OR l.line_tax_minor<0
   OR l.line_total_minor IS NULL OR l.line_total_minor<>l.line_net_minor+l.line_tax_minor
   OR l.tax_snapshot IS NULL OR jsonb_typeof(l.tax_snapshot)<>'object'
   OR jsonb_typeof(l.tax_snapshot->'category') IS DISTINCT FROM 'string'
   OR (l.tax_snapshot->>'category') COLLATE "C" !~ '^[a-z][a-z0-9_]{0,63}$' THEN
   RAISE EXCEPTION 'ORDER_INVALID_LINE' USING ERRCODE='22023';
  END IF;
  SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND id=l.reservation_id FOR UPDATE;
  -- OWNERSHIP_FENCE: no forged/reused/checkout reservation may enter an order.
  IF NOT FOUND OR r.owner_type<>'order' OR r.owner_id<>p_id OR r.sku_id<>l.sku_id OR r.quantity<>l.quantity THEN
   RAISE EXCEPTION 'ORDER_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501';
  END IF;
  IF r.state<>'active' OR r.expires_at<=clock_timestamp() THEN
   RAISE EXCEPTION 'ORDER_RESERVATION_INACTIVE' USING ERRCODE='55000';
  END IF;
  net:=net+l.line_net_minor; tax:=tax+l.line_tax_minor;
 END LOOP;
 FOR a IN SELECT * FROM jsonb_to_recordset(p_adjustments) AS x(id uuid,kind text,amount_minor bigint,reason text) LOOP
  IF a.id IS NULL OR a.kind IS NULL OR a.amount_minor IS NULL OR a.reason IS NULL OR btrim(a.reason)='' OR length(a.reason)>1000
   OR NOT ((a.kind='discount' AND a.amount_minor<0) OR (a.kind='surcharge' AND a.amount_minor>0)) THEN
   RAISE EXCEPTION 'ORDER_INVALID_ADJUSTMENT' USING ERRCODE='22023';
  END IF;
  adj:=adj+a.amount_minor;
 END LOOP;
 snapshot_total:=net+tax+p_shipping+adj;
 IF snapshot_total<0 THEN RAISE EXCEPTION 'ORDER_INVALID_TOTAL' USING ERRCODE='22023'; END IF;
 SELECT coalesce(max(order_number),0)+1 INTO number FROM app.orders WHERE tenant_id=t;
 INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,customer_id,guest_contact,currency,
  items_net_minor,tax_minor,shipping_minor,shipping_snapshot,adjustment_minor,total_minor)
 VALUES(t,p_id,number,p_key,p_hash,p_customer,p_guest,p_currency,net,tax,p_shipping,p_shipping_snapshot,adj,snapshot_total);
 INSERT INTO app.order_lines(tenant_id,id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
 SELECT t,x.id,p_id,x.sku_id,x.sku,x.quantity,x.unit_price_minor,x.line_net_minor,x.line_tax_minor,x.line_total_minor,x.reservation_id,x.tax_snapshot
 FROM jsonb_to_recordset(p_lines) AS x(id uuid,sku_id uuid,sku text,quantity bigint,unit_price_minor bigint,
  line_net_minor bigint,line_tax_minor bigint,line_total_minor bigint,reservation_id uuid,tax_snapshot jsonb);
 INSERT INTO app.order_adjustments(tenant_id,id,order_id,kind,amount_minor,reason)
 SELECT t,x.id,p_id,x.kind,x.amount_minor,x.reason FROM jsonb_to_recordset(p_adjustments) AS x(id uuid,kind text,amount_minor bigint,reason text);
 UPDATE app.orders SET sealed=true,updated_at=clock_timestamp() WHERE tenant_id=t AND id=p_id RETURNING * INTO o;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.created',jsonb_build_object('order_id',p_id,'order_number',number,'state',o.state,'version',o.version),
  'orders.events','created:'||p_id::text);
 RETURN o;
END $fn$;
