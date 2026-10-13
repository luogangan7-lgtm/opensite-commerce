-- stable-id: c18b-quote-conversion
-- Responsibility: quote-owned immutable order conversion receipts.
-- Dependencies: approved quotes027 and orders020 public commands; called by quotes.
-- Invariants: one quote/order/key binding, fresh company authority, DB-clock expiry,
-- exact negotiated snapshots, atomic outbox; never writes orders or inventory.
-- Notifications owns conversion fanout using the immutable receipt's customer.
CREATE TABLE app.quote_conversions (
 tenant_id uuid NOT NULL, quote_id uuid NOT NULL, buyer_company_id uuid NOT NULL,
 customer_id uuid NOT NULL, order_id uuid NOT NULL, quote_version bigint NOT NULL CHECK(quote_version>0),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128 AND btrim(idempotency_key)<>''),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 order_number bigint NOT NULL CHECK(order_number>0), currency char(3) NOT NULL CHECK(currency::text ~ '^[A-Z]{3}$'),
 total_minor bigint NOT NULL CHECK(total_minor>0), created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,quote_id), UNIQUE(tenant_id,order_id),
 CONSTRAINT quote_conversion_key UNIQUE(tenant_id,customer_id,idempotency_key),
 FOREIGN KEY(tenant_id,quote_id,buyer_company_id) REFERENCES app.quotes(tenant_id,id,buyer_company_id),
 FOREIGN KEY(tenant_id,buyer_company_id,customer_id) REFERENCES app.buyer_company_members(tenant_id,buyer_company_id,customer_id),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id)
);
ALTER TABLE app.quote_conversions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.quote_conversions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.quote_conversions TO app_runtime,app_quotes_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY quote_conversion_outbox_namespace ON app.outbox_events AS RESTRICTIVE FOR INSERT TO app_runtime
 WITH CHECK(NOT(delivery_target='quotes' AND starts_with(dedup_key,'converted:')));
REVOKE ALL ON app.quote_conversions FROM PUBLIC;
DO $acl$ DECLARE r text; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  EXECUTE format('REVOKE ALL ON app.quote_conversions FROM %I',r);
 END LOOP;
END $acl$;
GRANT SELECT ON app.quote_conversions TO app_runtime,app_quotes_executor;
GRANT INSERT(tenant_id,quote_id,buyer_company_id,customer_id,order_id,quote_version,idempotency_key,request_hash,order_number,currency,total_minor)
 ON app.quote_conversions TO app_quotes_executor;
GRANT SELECT(tenant_id,id,customer_id,order_number,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor,sealed,state)
 ON app.orders TO app_quotes_executor;
GRANT SELECT(tenant_id,order_id,sku_id,quantity,unit_price_minor,line_net_minor,line_tax_minor,line_total_minor,tax_snapshot)
 ON app.order_lines TO app_quotes_executor;
GRANT SELECT(tenant_id,order_id) ON app.order_adjustments TO app_quotes_executor;
CREATE POLICY quotes_conversion_orders ON app.orders TO app_quotes_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY quotes_conversion_lines ON app.order_lines TO app_quotes_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY quotes_conversion_adjustments ON app.order_adjustments TO app_quotes_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

CREATE FUNCTION app.quote_conversion_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'QUOTE_CONVERSION_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF current_user<>'app_quotes_executor' THEN RAISE EXCEPTION 'QUOTE_CONVERSION_COMMAND_REQUIRED' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER quote_conversion_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quote_conversions
 FOR EACH ROW EXECUTE FUNCTION app.quote_conversion_guard();

CREATE FUNCTION app.quote_conversion_lock(p_tenant uuid,p_quote uuid,p_company uuid,p_customer uuid)
RETURNS app.quotes LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE q app.quotes; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO q FROM app.quotes WHERE tenant_id=p_tenant AND id=p_quote FOR UPDATE;
 IF NOT FOUND OR q.buyer_company_id IS DISTINCT FROM p_company OR q.status='draft'
 OR NOT app.quote_actor_access(p_tenant,p_company,'customer',p_customer,'read') THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 -- Authorized history remains replayable after expiry; it cannot create again.
 IF NOT EXISTS(SELECT FROM app.quote_conversions WHERE tenant_id=p_tenant AND quote_id=p_quote)
 AND (q.status<>'approved' OR q.expires_at<=clock_timestamp()) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOT_APPROVED_OR_EXPIRED' USING ERRCODE='22023'; END IF;
 RETURN q;
END $$;

CREATE FUNCTION app.quote_conversion_bind(p_tenant uuid,p_quote uuid,p_company uuid,p_customer uuid,p_order uuid,
 p_quote_version bigint,p_idempotency_key text,p_request_hash bytea)
RETURNS app.quote_conversions LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE q app.quotes; c app.quote_conversions; o record; BEGIN
 q:=app.quote_conversion_lock(p_tenant,p_quote,p_company,p_customer);
 IF p_order IS NULL OR p_quote_version IS NULL OR p_idempotency_key IS NULL OR length(p_idempotency_key) NOT BETWEEN 1 AND 128
 OR btrim(p_idempotency_key)='' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_ARGUMENT' USING ERRCODE='22023'; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('quote-conversion-key:'||p_tenant::text||':'||p_customer::text||':'||p_idempotency_key,0));
 SELECT * INTO c FROM app.quote_conversions WHERE tenant_id=p_tenant AND customer_id=p_customer AND idempotency_key=p_idempotency_key;
 IF FOUND THEN
  IF c.quote_id<>p_quote OR c.order_id<>p_order OR c.quote_version<>p_quote_version OR c.request_hash<>p_request_hash THEN
   RAISE EXCEPTION 'QUOTE_CONVERSION_KEY_REUSED' USING ERRCODE='22000'; END IF;
  RETURN c;
 END IF;
 IF EXISTS(SELECT FROM app.quote_conversions WHERE tenant_id=p_tenant AND quote_id=p_quote) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_ALREADY_CONVERTED' USING ERRCODE='55000'; END IF;
 -- Expiry is checked again after key/quote/stock waits, immediately before binding.
 IF q.status<>'approved' OR q.expires_at<=clock_timestamp() THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOT_APPROVED_OR_EXPIRED' USING ERRCODE='22023'; END IF;
 IF q.version<>p_quote_version THEN RAISE EXCEPTION 'QUOTE_CONVERSION_VERSION' USING ERRCODE='P0003'; END IF;
 SELECT tenant_id,id,customer_id,order_number,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor,sealed,state
 INTO o FROM app.orders WHERE tenant_id=p_tenant AND id=p_order;
 IF NOT FOUND OR NOT o.sealed OR o.state<>'pending_payment' OR o.customer_id IS DISTINCT FROM p_customer
 OR o.currency<>q.currency OR o.items_net_minor<>q.subtotal OR o.tax_minor<>q.tax OR o.shipping_minor<>q.shipping
 OR o.total_minor<>q.total OR q.total<=0 OR o.shipping_snapshot IS DISTINCT FROM q.snapshot->'shipping'
 OR EXISTS(SELECT FROM app.order_adjustments WHERE tenant_id=p_tenant AND order_id=p_order)
 OR EXISTS(SELECT FROM app.quote_lines WHERE tenant_id=p_tenant AND quote_id=p_quote AND variant_id IS NULL) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_ORDER_SNAPSHOT' USING ERRCODE='23514'; END IF;
 -- EXCEPT ALL preserves duplicate multiplicities; equal totals do not prove equal lines.
 IF EXISTS(
  (SELECT variant_id,quantity,unit_price,line_total,(snapshot->'amounts'->>'tax')::bigint,
    (snapshot->'amounts'->>'gross')::bigint,snapshot->'snapshot'
   FROM app.quote_lines WHERE tenant_id=p_tenant AND quote_id=p_quote
   EXCEPT ALL SELECT sku_id,quantity,unit_price_minor,line_net_minor,line_tax_minor,line_total_minor,tax_snapshot
   FROM app.order_lines WHERE tenant_id=p_tenant AND order_id=p_order)
  UNION ALL
  (SELECT sku_id,quantity,unit_price_minor,line_net_minor,line_tax_minor,line_total_minor,tax_snapshot
   FROM app.order_lines WHERE tenant_id=p_tenant AND order_id=p_order
   EXCEPT ALL SELECT variant_id,quantity,unit_price,line_total,(snapshot->'amounts'->>'tax')::bigint,
    (snapshot->'amounts'->>'gross')::bigint,snapshot->'snapshot'
   FROM app.quote_lines WHERE tenant_id=p_tenant AND quote_id=p_quote)) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_LINE_SNAPSHOT' USING ERRCODE='23514'; END IF;
 INSERT INTO app.quote_conversions(tenant_id,quote_id,buyer_company_id,customer_id,order_id,quote_version,idempotency_key,request_hash,order_number,currency,total_minor)
 VALUES(p_tenant,p_quote,p_company,p_customer,p_order,p_quote_version,p_idempotency_key,p_request_hash,o.order_number,o.currency,o.total_minor)
 RETURNING * INTO c;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(p_tenant,'quote.converted',jsonb_build_object('quote_id',p_quote,'order_id',p_order,'quote_version',p_quote_version),
 'quotes','converted:'||p_quote::text);
 RETURN c;
END $$;

CREATE FUNCTION app.quote_conversion_get(p_tenant uuid,p_quote uuid,p_company uuid,p_kind text,p_actor uuid)
RETURNS app.quote_conversions LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE c app.quote_conversions; BEGIN
 SELECT * INTO c FROM app.quote_conversions WHERE tenant_id=p_tenant AND quote_id=p_quote;
 -- Absence is normal on first conversion: keep the caller transaction usable.
 IF NOT FOUND THEN RETURN NULL; END IF;
 IF (p_kind='customer' AND c.buyer_company_id IS DISTINCT FROM p_company)
 OR NOT app.quote_actor_access(p_tenant,c.buyer_company_id,p_kind,p_actor,'read') THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 RETURN c;
END $$;
GRANT CREATE ON SCHEMA app TO app_quotes_executor;
ALTER FUNCTION app.quote_conversion_guard() OWNER TO app_quotes_executor;
ALTER FUNCTION app.quote_conversion_lock(uuid,uuid,uuid,uuid) OWNER TO app_quotes_executor;
ALTER FUNCTION app.quote_conversion_bind(uuid,uuid,uuid,uuid,uuid,bigint,text,bytea) OWNER TO app_quotes_executor;
ALTER FUNCTION app.quote_conversion_get(uuid,uuid,uuid,text,uuid) OWNER TO app_quotes_executor;
REVOKE CREATE ON SCHEMA app FROM app_quotes_executor;
DO $acl$ DECLARE f record; r text; BEGIN
 FOR f IN SELECT p.oid::regprocedure AS sig FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='app' AND p.proname IN('quote_conversion_guard','quote_conversion_lock','quote_conversion_bind','quote_conversion_get') LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.sig);
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_quotes_executor' LOOP
   EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.sig,r);
  END LOOP;
 END LOOP;
END $acl$;
GRANT EXECUTE ON FUNCTION app.quote_conversion_lock(uuid,uuid,uuid,uuid),
 app.quote_conversion_bind(uuid,uuid,uuid,uuid,uuid,bigint,text,bytea),app.quote_conversion_get(uuid,uuid,uuid,text,uuid) TO app_runtime;

-- Orders retains the inventory write boundary. This preparation port holds all
-- locks through the caller's capture transaction and decides expiry after waits.
CREATE FUNCTION app.orders_validate_payment_reservations(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; BEGIN
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'ORDER_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF NOT o.sealed OR o.state<>'pending_payment' THEN RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000'; END IF;
 PERFORM ir.id FROM app.inventory_reservations ir JOIN app.order_lines ol
 ON(ol.tenant_id,ol.reservation_id)=(ir.tenant_id,ir.id)
 WHERE ol.tenant_id=t AND ol.order_id=p_id ORDER BY ir.id FOR UPDATE OF ir;
 PERFORM s.sku_id FROM app.stock_levels s JOIN app.inventory_reservations ir
 ON(ir.tenant_id,ir.warehouse_id,ir.sku_id)=(s.tenant_id,s.warehouse_id,s.sku_id)
 JOIN app.order_lines ol ON(ol.tenant_id,ol.reservation_id)=(ir.tenant_id,ir.id)
 WHERE ol.tenant_id=t AND ol.order_id=p_id ORDER BY s.warehouse_id,s.sku_id FOR UPDATE OF s;
 IF EXISTS(SELECT FROM app.order_lines ol LEFT JOIN app.inventory_reservations ir
 ON(ir.tenant_id,ir.id)=(ol.tenant_id,ol.reservation_id)
 WHERE ol.tenant_id=t AND ol.order_id=p_id AND(ir.id IS NULL OR ir.owner_type<>'order' OR ir.owner_id<>p_id
 OR ir.sku_id<>ol.sku_id OR ir.quantity<>ol.quantity)) THEN
  RAISE EXCEPTION 'ORDER_RESERVATION_OWNER_MISMATCH' USING ERRCODE='42501'; END IF;
 IF EXISTS(SELECT FROM app.order_lines ol JOIN app.inventory_reservations ir
 ON(ir.tenant_id,ir.id)=(ol.tenant_id,ol.reservation_id)
 WHERE ol.tenant_id=t AND ol.order_id=p_id AND(ir.state<>'active' OR ir.expires_at<=clock_timestamp())) THEN
  RAISE EXCEPTION 'ORDER_RESERVATION_INACTIVE' USING ERRCODE='55000'; END IF;
END $$;
-- Row locks require one UPDATE column; the command never changes stock data.
GRANT SELECT(tenant_id,warehouse_id,sku_id) ON app.stock_levels TO app_orders_executor;
GRANT UPDATE(sku_id) ON app.stock_levels TO app_orders_executor;
CREATE POLICY orders_preflight_stock ON app.stock_levels TO app_orders_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT CREATE ON SCHEMA app TO app_orders_executor;
ALTER FUNCTION app.orders_validate_payment_reservations(uuid) OWNER TO app_orders_executor;
REVOKE CREATE ON SCHEMA app FROM app_orders_executor;
REVOKE ALL ON FUNCTION app.orders_validate_payment_reservations(uuid) FROM PUBLIC;
DO $acl$ DECLARE r text; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_orders_executor' LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION app.orders_validate_payment_reservations(uuid) FROM %I',r);
 END LOOP;
END $acl$;
GRANT EXECUTE ON FUNCTION app.orders_validate_payment_reservations(uuid) TO app_runtime;

-- Forward integration: existing notification lifecycle commands remain unchanged.
ALTER TABLE app.notification_deliveries DROP CONSTRAINT notification_deliveries_template_key_check;
ALTER TABLE app.notification_deliveries ADD CONSTRAINT notification_deliveries_template_key_check
 CHECK(template_key IN('order.paid','order.shipped','order.refund','quote.sent','quote.approved','quote.converted'));
GRANT SELECT(tenant_id,quote_id,order_id,quote_version,customer_id)
 ON app.quote_conversions TO app_notifications_executor;
CREATE POLICY notifications_quote_conversions ON app.quote_conversions TO app_notifications_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

-- Private notification-owned trigger: source, delivery and dispatch share one tx.
-- Authority is the receipt/order tuple; payload contact fields never choose email.
CREATE FUNCTION app.notification_quote_conversion_fanout() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE email text; ref uuid; nid uuid:=uuidv7(); dispatch uuid:=uuidv7(); BEGIN
 IF NEW.delivery_target<>'quotes' OR NEW.event_type<>'quote.converted' THEN RETURN NEW; END IF;
 SELECT c.email_normalized,r.quote_id INTO email,ref FROM app.quote_conversions r
 JOIN app.orders o ON(o.tenant_id,o.id,o.customer_id)=(r.tenant_id,r.order_id,r.customer_id)
 LEFT JOIN app.customers c ON(c.tenant_id,c.id)=(r.tenant_id,r.customer_id)
 WHERE r.tenant_id=NEW.tenant_id AND r.quote_id::text=NEW.payload->>'quote_id'
 AND r.order_id::text=NEW.payload->>'order_id' AND r.quote_version::text=NEW.payload->>'quote_version'
 AND NEW.dedup_key='converted:'||r.quote_id::text;
 IF NOT FOUND THEN RETURN NEW; END IF;
 email:=lower(btrim(email));
 -- An unavailable owner contact leaves a durable failure without external dispatch.
 IF email IS NULL OR length(email)>254 OR email COLLATE "C" !~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+([.][A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+)*@[A-Za-z0-9-]+([.][A-Za-z0-9-]+)+$' THEN
  INSERT INTO app.notification_deliveries(tenant_id,id,source_event_id,recipient,locale,template_key,reference_id,status,last_error)
  VALUES(NEW.tenant_id,nid,NEW.id,'','en','quote.converted',ref,'dead_letter','NOTIFICATION_RECIPIENT_UNAVAILABLE')
  ON CONFLICT(tenant_id,source_event_id,recipient,template_key) DO NOTHING;
  RETURN NEW;
 END IF;
 INSERT INTO app.notification_deliveries(tenant_id,id,source_event_id,dispatch_event_id,recipient,locale,template_key,reference_id)
 VALUES(NEW.tenant_id,nid,NEW.id,dispatch,email,'en','quote.converted',ref)
 ON CONFLICT(tenant_id,source_event_id,recipient,template_key) DO NOTHING;
 IF FOUND THEN
  INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key)
  VALUES(NEW.tenant_id,dispatch,'notification.dispatch',jsonb_build_object('notification_id',nid),'notifications.email',nid::text||':1');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER notification_quote_conversion_source AFTER INSERT ON app.outbox_events
 FOR EACH ROW EXECUTE FUNCTION app.notification_quote_conversion_fanout();
GRANT CREATE ON SCHEMA app TO app_notifications_executor;
ALTER FUNCTION app.notification_quote_conversion_fanout() OWNER TO app_notifications_executor;
REVOKE CREATE ON SCHEMA app FROM app_notifications_executor;
REVOKE ALL ON FUNCTION app.notification_quote_conversion_fanout() FROM PUBLIC;
DO $acl$ DECLARE r text; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_notifications_executor' LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION app.notification_quote_conversion_fanout() FROM %I',r);
 END LOOP;
END $acl$;
