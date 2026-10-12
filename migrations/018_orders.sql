-- Responsibility: immutable order snapshots and fenced order/inventory transitions.
-- Depends on catalog, inventory and reliability; called by orders' trusted service.
-- Invariants: tenant RLS, sealed snapshots, exact reservation ownership, unique events.
-- Customer references are tenant-bound; guest contact is an immutable alternative. No HTTP/payment PSP.
DO $roles$
DECLARE member_name text;
BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_orders_executor') THEN
  CREATE ROLE app_orders_executor NOLOGIN;
 END IF;
 ALTER ROLE app_orders_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 FOR member_name IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid
 WHERE m.member='app_orders_executor'::regrole LOOP
  EXECUTE format('REVOKE %I FROM app_orders_executor CASCADE',member_name);
 END LOOP;
 FOR member_name IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member
 WHERE m.roleid='app_orders_executor'::regrole LOOP
  EXECUTE format('REVOKE app_orders_executor FROM %I CASCADE',member_name);
 END LOOP;
END $roles$;
GRANT USAGE ON SCHEMA app TO app_orders_executor;
REVOKE CREATE ON SCHEMA app FROM app_orders_executor;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_orders_executor;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM app_orders_executor;
-- Reused role names must not retain old column ACLs either.
DO $columns$
DECLARE t record; cols text;
BEGIN
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO cols
   FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM app_orders_executor',cols,cols,cols,cols,t.relname);
 END LOOP;
END $columns$;

CREATE TABLE app.orders(
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), id uuid NOT NULL,
 order_number bigint NOT NULL CHECK(order_number>0),
 creation_key text NOT NULL CHECK(length(creation_key) BETWEEN 1 AND 128 AND btrim(creation_key)<>''),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 customer_id uuid, guest_contact jsonb,
 currency char(3) NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 items_net_minor bigint NOT NULL CHECK(items_net_minor>=0),
 tax_minor bigint NOT NULL CHECK(tax_minor>=0),
 shipping_minor bigint NOT NULL CHECK(shipping_minor>=0),
 shipping_snapshot jsonb NOT NULL CHECK(jsonb_typeof(shipping_snapshot)='object'),
 adjustment_minor bigint NOT NULL DEFAULT 0,
 total_minor bigint NOT NULL CHECK(total_minor>=0),
 refunded_minor bigint NOT NULL DEFAULT 0 CHECK(refunded_minor>=0 AND refunded_minor<=total_minor),
 state text NOT NULL DEFAULT 'pending_payment' CHECK(state IN('pending_payment','paid','partially_fulfilled','fulfilled','cancelled','partially_refunded','refunded')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), sealed boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,order_number), UNIQUE(tenant_id,creation_key),
 CONSTRAINT orders_customer_fk FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 CHECK((customer_id IS NOT NULL AND guest_contact IS NULL) OR
  (customer_id IS NULL AND guest_contact IS NOT NULL AND jsonb_typeof(guest_contact)='object'
   AND nullif(btrim(guest_contact->>'email'),'') IS NOT NULL AND length(guest_contact->>'email')<=320)),
 CHECK(total_minor=items_net_minor+tax_minor+shipping_minor+adjustment_minor),
 CHECK((state IN('pending_payment','paid','partially_fulfilled','fulfilled','cancelled') AND refunded_minor=0)
  OR (state='partially_refunded' AND refunded_minor>0 AND refunded_minor<total_minor)
  OR (state='refunded' AND refunded_minor=total_minor))
);
CREATE TABLE app.order_lines(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), order_id uuid NOT NULL,
 sku_id uuid NOT NULL, sku text NOT NULL CHECK(btrim(sku)<>'' AND length(sku)<=255),
 quantity bigint NOT NULL CHECK(quantity>0), unit_price_minor bigint NOT NULL CHECK(unit_price_minor>=0),
 line_net_minor bigint NOT NULL CHECK(line_net_minor>=0), line_tax_minor bigint NOT NULL CHECK(line_tax_minor>=0),
 line_total_minor bigint NOT NULL CHECK(line_total_minor=line_net_minor+line_tax_minor),
 reservation_id uuid NOT NULL, tax_snapshot jsonb NOT NULL CHECK(jsonb_typeof(tax_snapshot)='object'),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,reservation_id),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 FOREIGN KEY(tenant_id,sku_id) REFERENCES app.product_variants(tenant_id,id),
 FOREIGN KEY(tenant_id,reservation_id) REFERENCES app.inventory_reservations(tenant_id,id)
);
CREATE INDEX order_lines_order ON app.order_lines(tenant_id,order_id,id);
CREATE TABLE app.order_adjustments(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), order_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN('discount','surcharge')), amount_minor bigint NOT NULL,
 reason text NOT NULL CHECK(btrim(reason)<>'' AND length(reason)<=1000),
 PRIMARY KEY(tenant_id,id), FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 CHECK((kind='discount' AND amount_minor<0) OR (kind='surcharge' AND amount_minor>0))
);
CREATE INDEX order_adjustments_order ON app.order_adjustments(tenant_id,order_id,id);
DO $rls$
DECLARE name text;
BEGIN
 FOREACH name IN ARRAY ARRAY['orders','order_lines','order_adjustments'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',name);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_orders_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',name);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime,app_orders_executor',name);
  EXECUTE format('GRANT SELECT ON app.%I TO app_runtime,app_orders_executor',name);
  EXECUTE format('GRANT INSERT ON app.%I TO app_orders_executor',name);
 END LOOP;
END $rls$;
GRANT UPDATE(state,version,sealed,refunded_minor,updated_at) ON app.orders TO app_orders_executor;
GRANT SELECT ON app.tenants,app.inventory_reservations,app.outbox_events TO app_orders_executor;
-- PostgreSQL row locks need one UPDATE column. IDs are never modified by commands.
GRANT UPDATE(id) ON app.inventory_reservations TO app_orders_executor;
GRANT INSERT(tenant_id,id,event_type,payload,delivery_target,dedup_key,created_at)
 ON app.outbox_events TO app_orders_executor;
CREATE POLICY orders_reservations ON app.inventory_reservations TO app_orders_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY orders_outbox ON app.outbox_events TO app_orders_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
-- Runtime cannot forge an order transition's dedup receipt in the shared outbox.
CREATE POLICY orders_outbox_namespace ON app.outbox_events AS RESTRICTIVE FOR INSERT TO app_runtime
 WITH CHECK(delivery_target<>'orders.events');
-- Reservation terminal commands are capability-private. Orders validates owner/SKU/qty.
REVOKE EXECUTE ON FUNCTION app.inventory_commit(uuid),app.inventory_release(uuid) FROM app_runtime;
GRANT EXECUTE ON FUNCTION app.inventory_commit(uuid),app.inventory_release(uuid) TO app_orders_executor;

CREATE FUNCTION app.orders_context() RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid;
BEGIN
 BEGIN t:=nullif(current_setting('app.tenant_id',true),'')::uuid;
 EXCEPTION WHEN invalid_text_representation THEN
  RAISE EXCEPTION 'ORDERS_TENANT_CONTEXT_REQUIRED' USING ERRCODE='42501';
 END;
 IF t IS NULL OR NOT EXISTS(SELECT FROM app.tenants WHERE id=t AND status='active') THEN
  RAISE EXCEPTION 'ORDERS_TENANT_CONTEXT_REQUIRED' USING ERRCODE='42501';
 END IF;
 RETURN t;
END $fn$;
CREATE FUNCTION app.orders_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF TG_OP='INSERT' THEN
  IF current_user<>'app_orders_executor' OR NEW.state<>'pending_payment' OR NEW.version<>1
   OR NEW.sealed OR NEW.refunded_minor<>0 THEN
   RAISE EXCEPTION 'ORDER_CONTROLLED_COMMAND_REQUIRED' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF (to_jsonb(NEW)-ARRAY['state','version','sealed','refunded_minor','updated_at']) IS DISTINCT FROM
    (to_jsonb(OLD)-ARRAY['state','version','sealed','refunded_minor','updated_at'])
   OR (OLD.sealed AND NOT NEW.sealed) THEN
  RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 IF (OLD.state IS DISTINCT FROM NEW.state OR OLD.refunded_minor IS DISTINCT FROM NEW.refunded_minor
  OR OLD.sealed IS DISTINCT FROM NEW.sealed OR OLD.version IS DISTINCT FROM NEW.version)
  AND current_user<>'app_orders_executor' THEN
  RAISE EXCEPTION 'ORDER_CONTROLLED_COMMAND_REQUIRED' USING ERRCODE='55000';
 END IF;
 IF OLD.state IS DISTINCT FROM NEW.state OR OLD.refunded_minor IS DISTINCT FROM NEW.refunded_minor THEN
  IF NOT OLD.sealed OR NOT NEW.sealed OR NEW.version<>OLD.version+1 OR NOT (
   (OLD.state='pending_payment' AND NEW.state IN('paid','cancelled'))
   OR (OLD.state='paid' AND NEW.state IN('partially_fulfilled','fulfilled','partially_refunded','refunded'))
   OR (OLD.state='partially_fulfilled' AND NEW.state IN('fulfilled','partially_refunded','refunded'))
   OR (OLD.state='fulfilled' AND NEW.state IN('partially_refunded','refunded'))
   OR (OLD.state='partially_refunded' AND NEW.state IN('partially_refunded','refunded')))
   OR NEW.refunded_minor<OLD.refunded_minor
   OR (NEW.state IN('partially_refunded','refunded') AND NEW.refunded_minor<=OLD.refunded_minor) THEN
   RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000';
  END IF;
 ELSE
  IF NEW.version<>OLD.version THEN RAISE EXCEPTION 'ORDER_INVALID_VERSION' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER orders_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON app.orders
 FOR EACH ROW EXECUTE FUNCTION app.orders_guard();
CREATE FUNCTION app.orders_child_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE o app.orders;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000'; END IF;
 SELECT * INTO STRICT o FROM app.orders WHERE tenant_id=NEW.tenant_id AND id=NEW.order_id FOR UPDATE;
 IF o.sealed THEN RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000'; END IF;
 -- A real parent write prevents stale REPEATABLE READ snapshots missing a child.
 UPDATE app.orders SET updated_at=clock_timestamp() WHERE tenant_id=NEW.tenant_id AND id=NEW.order_id;
 RETURN NEW;
END $fn$;
CREATE TRIGGER order_lines_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON app.order_lines
 FOR EACH ROW EXECUTE FUNCTION app.orders_child_guard();
CREATE TRIGGER order_adjustments_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON app.order_adjustments
 FOR EACH ROW EXECUTE FUNCTION app.orders_child_guard();

CREATE FUNCTION app.orders_create(p_id uuid,p_key text,p_hash bytea,p_customer uuid,p_guest jsonb,
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
   OR l.tax_snapshot IS NULL OR jsonb_typeof(l.tax_snapshot)<>'object' THEN
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

CREATE FUNCTION app.orders_transition(p_id uuid,p_target text,p_event_key text,p_refund_minor bigint DEFAULT 0)
RETURNS app.orders LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.orders_context(); o app.orders; e app.outbox_events; l app.order_lines; r app.inventory_reservations;
 refunded bigint;
BEGIN
 IF p_id IS NULL OR p_target IS NULL OR p_event_key IS NULL OR btrim(p_event_key)='' OR length(p_event_key)>128
  OR p_refund_minor IS NULL OR p_refund_minor<0 THEN
  RAISE EXCEPTION 'ORDER_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'ORDER_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO e FROM app.outbox_events WHERE tenant_id=t AND delivery_target='orders.events'
  AND dedup_key='transition:'||p_id::text||':'||p_event_key;
 IF FOUND THEN
  IF e.payload->>'target'<>p_target OR (e.payload->>'refund_minor')::bigint<>p_refund_minor THEN
   RAISE EXCEPTION 'ORDER_EVENT_KEY_REUSED' USING ERRCODE='22023';
  END IF;
  RETURN o;
 END IF;
 IF NOT o.sealed OR NOT (
  (o.state='pending_payment' AND p_target IN('paid','cancelled'))
  OR (o.state='paid' AND p_target IN('partially_fulfilled','fulfilled','partially_refunded','refunded'))
  OR (o.state='partially_fulfilled' AND p_target IN('fulfilled','partially_refunded','refunded'))
  OR (o.state='fulfilled' AND p_target IN('partially_refunded','refunded'))
  OR (o.state='partially_refunded' AND p_target IN('partially_refunded','refunded'))) THEN
  RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000';
 END IF;
 refunded:=o.refunded_minor+p_refund_minor;
 IF (p_target IN('partially_refunded','refunded') AND (p_refund_minor<=0 OR refunded>o.total_minor
   OR (p_target='partially_refunded' AND refunded>=o.total_minor) OR (p_target='refunded' AND refunded<>o.total_minor)))
   OR (p_target NOT IN('partially_refunded','refunded') AND p_refund_minor<>0) THEN
  RAISE EXCEPTION 'ORDER_INVALID_REFUND' USING ERRCODE='22023';
 END IF;
 IF p_target IN('paid','cancelled') THEN
  -- Prelock every reservation before any stock write. Expiry's SKIP LOCKED
  -- sweep cannot hold a later reservation while waiting for our earlier stock.
  PERFORM ir.id FROM app.inventory_reservations ir
   JOIN app.order_lines ol ON ol.tenant_id=ir.tenant_id AND ol.reservation_id=ir.id
   WHERE ol.tenant_id=t AND ol.order_id=p_id ORDER BY ir.id FOR UPDATE OF ir;
  -- All finish operations then take stock locks in warehouse/SKU order.
  FOR l IN SELECT ol.* FROM app.order_lines ol
   JOIN app.inventory_reservations ir ON ir.tenant_id=ol.tenant_id AND ir.id=ol.reservation_id
   WHERE ol.tenant_id=t AND ol.order_id=p_id ORDER BY ir.warehouse_id,ir.sku_id,ol.reservation_id LOOP
   SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND id=l.reservation_id FOR UPDATE;
   -- OWNERSHIP_FENCE: repeat checks at transition, never trust the incoming ID alone.
   IF NOT FOUND OR r.owner_type<>'order' OR r.owner_id<>p_id OR r.sku_id<>l.sku_id OR r.quantity<>l.quantity THEN
    RAISE EXCEPTION 'ORDER_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501';
   END IF;
   IF p_target='paid' THEN
    IF r.state<>'active' THEN RAISE EXCEPTION 'ORDER_RESERVATION_INACTIVE' USING ERRCODE='55000'; END IF;
    PERFORM app.inventory_commit(r.id);
   ELSIF r.state='active' THEN PERFORM app.inventory_release(r.id);
   ELSIF r.state NOT IN('released','expired') THEN
    RAISE EXCEPTION 'ORDER_RESERVATION_INACTIVE' USING ERRCODE='55000';
   END IF;
  END LOOP;
 END IF;
 UPDATE app.orders SET state=p_target,version=version+1,refunded_minor=refunded,updated_at=clock_timestamp()
 WHERE tenant_id=t AND id=p_id RETURNING * INTO o;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.'||p_target,jsonb_build_object('order_id',p_id,'target',p_target,'refund_minor',p_refund_minor,'version',o.version),
  'orders.events','transition:'||p_id::text||':'||p_event_key);
 RETURN o;
END $fn$;
-- ALTER OWNER needs CREATE briefly; revoke it immediately after provisioning.
GRANT CREATE ON SCHEMA app TO app_orders_executor;
DO $functions$
DECLARE f record;
BEGIN
 FOR f IN SELECT p.oid::regprocedure AS identity FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='app' AND p.proname=ANY(ARRAY['orders_context','orders_guard','orders_child_guard','orders_create','orders_transition']) LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_orders_executor',f.identity);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC,app_runtime',f.identity);
 END LOOP;
END $functions$;
REVOKE CREATE ON SCHEMA app FROM app_orders_executor;
GRANT EXECUTE ON FUNCTION app.orders_create(uuid,text,bytea,uuid,jsonb,text,bigint,jsonb,jsonb,jsonb),
 app.orders_transition(uuid,text,text,bigint) TO app_runtime;
