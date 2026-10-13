-- Responsibility: bounded shipment allocation and durable RMA/refund receipts.
-- Dependencies: Orders owns state, Inventory owns restoration, Ledger owns posting.
-- Called by tenant-authorized fulfillment/returns coordinators via generated SQL.
-- Invariants: paid stock semantics unchanged, quantity fences are real parent writes,
-- immutable receipts precede payment effects; all stored monetary values are bigint.
-- Rollback: forward repair; never delete financial/stock/shipment history.
DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_fulfillment_executor') THEN CREATE ROLE app_fulfillment_executor; END IF;
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_returns_service') THEN CREATE ROLE app_returns_service; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member IN('app_fulfillment_executor'::regrole,'app_returns_service'::regrole)
  OR roleid IN('app_fulfillment_executor'::regrole,'app_returns_service'::regrole)) THEN
  RAISE EXCEPTION 'FULFILLMENT_ROLE_MEMBERSHIP_UNSAFE';
 END IF;
END $$;
ALTER ROLE app_fulfillment_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
ALTER ROLE app_returns_service NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_fulfillment_executor,app_returns_service;
REVOKE CREATE ON SCHEMA app FROM app_fulfillment_executor,app_returns_service;
ALTER TABLE app.order_lines ADD CONSTRAINT order_lines_order_identity UNIQUE(tenant_id,order_id,id);
ALTER TABLE app.orders ADD CONSTRAINT orders_currency_identity UNIQUE(tenant_id,id,currency);
CREATE TABLE app.shipments(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), order_id uuid NOT NULL,
 creation_key text NOT NULL CHECK(length(creation_key) BETWEEN 1 AND 128 AND btrim(creation_key)<>''),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 carrier text NOT NULL CHECK(length(carrier) BETWEEN 1 AND 128 AND btrim(carrier)<>''),
 tracking_number text NOT NULL CHECK(length(tracking_number)<=256),
 status text NOT NULL DEFAULT 'shipped' CHECK(status='shipped'),
 order_version bigint NOT NULL DEFAULT 1 CHECK(order_version>0), sealed boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),UNIQUE(tenant_id,id,order_id),UNIQUE(tenant_id,creation_key),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id)
);
CREATE TABLE app.shipment_lines(
 tenant_id uuid NOT NULL,id uuid NOT NULL DEFAULT uuidv7(),shipment_id uuid NOT NULL,
 order_id uuid NOT NULL,order_line_id uuid NOT NULL,quantity bigint NOT NULL CHECK(quantity>0),
 PRIMARY KEY(tenant_id,id),UNIQUE(tenant_id,shipment_id,order_line_id),UNIQUE(tenant_id,id,order_line_id),
 FOREIGN KEY(tenant_id,shipment_id,order_id) REFERENCES app.shipments(tenant_id,id,order_id),
 FOREIGN KEY(tenant_id,order_id,order_line_id) REFERENCES app.order_lines(tenant_id,order_id,id)
);
CREATE INDEX shipment_lines_order_line ON app.shipment_lines(tenant_id,order_line_id);
CREATE TABLE app.returns(
 tenant_id uuid NOT NULL,id uuid NOT NULL DEFAULT uuidv7(),order_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN('shipment_return','unshipped_cancel')),
 creation_key text NOT NULL CHECK(length(creation_key) BETWEEN 1 AND 128 AND btrim(creation_key)<>''),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND btrim(reason)<>''),
 currency char(3) NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 income_minor bigint NOT NULL DEFAULT 0 CHECK(income_minor>=0),
 tax_minor bigint NOT NULL DEFAULT 0 CHECK(tax_minor>=0),
 shipping_minor bigint NOT NULL DEFAULT 0 CHECK(shipping_minor>=0),
 refund_minor bigint NOT NULL DEFAULT 0 CHECK(refund_minor=income_minor+tax_minor+shipping_minor),
 status text NOT NULL CHECK(status IN('requested','received','refund_pending','refunded','completed')),
 order_version bigint NOT NULL DEFAULT 1 CHECK(order_version>0),sealed boolean NOT NULL DEFAULT false,
 restocked boolean NOT NULL DEFAULT false,received_at timestamptz,
 creation_response jsonb CHECK(creation_response IS NULL OR jsonb_typeof(creation_response)='object'),
 receive_response jsonb CHECK(receive_response IS NULL OR jsonb_typeof(receive_response)='object'),
 receive_key text,receive_hash bytea CHECK(receive_hash IS NULL OR octet_length(receive_hash)=32),
 refund_key text,refund_hash bytea CHECK(refund_hash IS NULL OR octet_length(refund_hash)=32),
 refund_expected_version bigint CHECK(refund_expected_version IS NULL OR refund_expected_version>0),
 provider_ref text CHECK(provider_ref IS NULL OR length(provider_ref) BETWEEN 1 AND 256),
 payment_version bigint CHECK(payment_version IS NULL OR payment_version>0),
 income_account_id uuid,tax_account_id uuid,shipping_account_id uuid,cash_account_id uuid,
 ledger_transaction_id uuid,refunded_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),UNIQUE(tenant_id,id,order_id),UNIQUE(tenant_id,creation_key),
 UNIQUE(tenant_id,receive_key),UNIQUE(tenant_id,refund_key),
 FOREIGN KEY(tenant_id,order_id,currency) REFERENCES app.orders(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,income_account_id,currency) REFERENCES app.ledger_accounts(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,tax_account_id,currency) REFERENCES app.ledger_accounts(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,shipping_account_id,currency) REFERENCES app.ledger_accounts(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,cash_account_id,currency) REFERENCES app.ledger_accounts(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,ledger_transaction_id,currency) REFERENCES app.ledger_transactions(tenant_id,id,currency),
 CHECK((status='requested')=(received_at IS NULL)),CHECK(NOT restocked OR received_at IS NOT NULL),
 CHECK((status IN('refund_pending','refunded'))=(refund_key IS NOT NULL)),
 CHECK(refund_key IS NULL OR (length(refund_key) BETWEEN 1 AND 128 AND btrim(refund_key)<>'' AND refund_hash IS NOT NULL
  AND provider_ref IS NOT NULL AND payment_version IS NOT NULL AND refund_expected_version IS NOT NULL
  AND income_account_id IS NOT NULL AND tax_account_id IS NOT NULL AND shipping_account_id IS NOT NULL
  AND cash_account_id IS NOT NULL AND refund_minor>0)),
 CHECK((status='refunded')=(ledger_transaction_id IS NOT NULL AND refunded_at IS NOT NULL)),
 CHECK(status<>'completed' OR (refund_minor=0 AND refund_key IS NULL AND ledger_transaction_id IS NULL AND refunded_at IS NULL))
);
CREATE INDEX returns_order ON app.returns(tenant_id,order_id);
CREATE TABLE app.return_lines(
 tenant_id uuid NOT NULL,id uuid NOT NULL DEFAULT uuidv7(),return_id uuid NOT NULL,order_id uuid NOT NULL,
 order_line_id uuid NOT NULL,shipment_line_id uuid,quantity bigint NOT NULL CHECK(quantity>0),
 income_minor bigint NOT NULL CHECK(income_minor>=0),tax_minor bigint NOT NULL CHECK(tax_minor>=0),
 shipping_minor bigint NOT NULL CHECK(shipping_minor>=0),
 PRIMARY KEY(tenant_id,id),UNIQUE NULLS NOT DISTINCT(tenant_id,return_id,order_line_id,shipment_line_id),
 FOREIGN KEY(tenant_id,return_id,order_id) REFERENCES app.returns(tenant_id,id,order_id),
 FOREIGN KEY(tenant_id,order_id,order_line_id) REFERENCES app.order_lines(tenant_id,order_id,id),
 FOREIGN KEY(tenant_id,shipment_line_id,order_line_id) REFERENCES app.shipment_lines(tenant_id,id,order_line_id)
);
CREATE INDEX return_lines_order_line ON app.return_lines(tenant_id,order_line_id);
CREATE INDEX return_lines_shipment_line ON app.return_lines(tenant_id,shipment_line_id);
CREATE UNIQUE INDEX inventory_return_restore_identity ON app.inventory_movements(tenant_id,owner_id,sku_id)
 WHERE owner_type='return' AND kind='inbound';
DO $tables$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['shipments','shipment_lines','returns','return_lines'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',name);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime,app_fulfillment_executor,app_returns_service',name);
  EXECUTE format('GRANT SELECT ON app.%I TO app_runtime,app_fulfillment_executor,app_orders_executor',name);
  EXECUTE format('GRANT INSERT ON app.%I TO app_fulfillment_executor',name);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_fulfillment_executor,app_returns_service,app_orders_executor,app_inventory_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',name);
 END LOOP;
END $tables$;
GRANT SELECT ON app.returns,app.return_lines TO app_returns_service,app_inventory_executor;
GRANT UPDATE(sealed,order_version) ON app.shipments TO app_fulfillment_executor;
GRANT UPDATE(creation_response,receive_response,sealed,order_version,status,restocked,received_at,receive_key,receive_hash,refund_key,refund_hash,refund_expected_version,provider_ref,payment_version,
 income_account_id,tax_account_id,shipping_account_id,cash_account_id,ledger_transaction_id,refunded_at,
 income_minor,tax_minor,shipping_minor,refund_minor) ON app.returns TO app_fulfillment_executor;
GRANT SELECT ON app.tenants,app.orders,app.order_lines,app.ledger_accounts,app.ledger_transactions,app.ledger_entries TO app_fulfillment_executor;
GRANT SELECT(tenant_id,id,total_minor,currency) ON app.orders TO app_returns_service;
-- The trusted refund transaction completes an already claimed C04 response
-- atomically with its journal and refund. It cannot claim/create another intent.
GRANT SELECT(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,claim_token,state)
 ON app.idempotency_records TO app_returns_service;
GRANT UPDATE(state,response_status,response_body_reference,completed_at)
 ON app.idempotency_records TO app_returns_service;
CREATE POLICY returns_service_idempotency_read ON app.idempotency_records FOR SELECT TO app_returns_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND operation='refundReturn');
CREATE POLICY returns_service_idempotency_complete ON app.idempotency_records FOR UPDATE TO app_returns_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND operation='refundReturn')
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND operation='refundReturn');
GRANT SELECT ON app.order_lines TO app_inventory_executor;
DO $foreign_read$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['orders','order_lines','ledger_accounts','ledger_transactions','ledger_entries'] LOOP
  EXECUTE format('CREATE POLICY fulfillment_read ON app.%I FOR SELECT TO app_fulfillment_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',name);
 END LOOP;
END $foreign_read$;
CREATE POLICY returns_service_order_read ON app.orders FOR SELECT TO app_returns_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY inventory_return_line_read ON app.order_lines FOR SELECT TO app_inventory_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

CREATE OR REPLACE FUNCTION app.orders_guard() RETURNS trigger LANGUAGE plpgsql
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
   OR (NEW.state IN('partially_refunded','refunded') AND OLD.state IS DISTINCT FROM NEW.state AND NEW.refunded_minor<=OLD.refunded_minor) THEN
   RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000';
  END IF;
 ELSE
  IF NEW.version<>OLD.version AND NOT(current_user='app_orders_executor' AND NEW.version=OLD.version+1) THEN
   RAISE EXCEPTION 'ORDER_INVALID_VERSION' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $fn$;

-- Only this closed Orders-owned capability may fence/version fulfillment work.
CREATE FUNCTION app.orders_fulfillment_fence(p_id uuid,p_expected bigint) RETURNS app.orders
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders;
BEGIN
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_ORDER_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_expected IS NOT NULL AND (p_expected<=0 OR o.version<>p_expected) THEN
  RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 -- QUANTITY_SNAPSHOT_FENCE: lock alone is insufficient at REPEATABLE READ.
 UPDATE app.orders SET updated_at=clock_timestamp() WHERE tenant_id=t AND id=p_id RETURNING * INTO o;
 RETURN o;
END $$;
CREATE FUNCTION app.orders_fulfillment_advance(p_id uuid,p_event text) RETURNS app.orders
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; target text; has_shipped boolean; all_shipped boolean;
BEGIN
 SELECT * INTO STRICT o FROM app.orders WHERE tenant_id=t AND id=p_id FOR UPDATE;
 SELECT EXISTS(SELECT FROM app.shipment_lines WHERE tenant_id=t AND order_id=p_id),
  NOT EXISTS(SELECT FROM app.order_lines l WHERE l.tenant_id=t AND l.order_id=p_id AND
   coalesce((SELECT sum(s.quantity) FROM app.shipment_lines s WHERE s.tenant_id=t AND s.order_line_id=l.id),0)<>l.quantity)
 INTO has_shipped,all_shipped;
 target:=CASE WHEN o.state IN('partially_refunded','refunded') THEN o.state WHEN all_shipped THEN 'fulfilled'
  WHEN has_shipped THEN 'partially_fulfilled' ELSE 'paid' END;
 UPDATE app.orders SET state=target,version=version+1,updated_at=clock_timestamp()
 WHERE tenant_id=t AND id=p_id RETURNING * INTO o;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.fulfillment_updated',jsonb_build_object('order_id',p_id,'version',o.version,'state',o.state),
  'orders.events','fulfillment:'||p_id::text||':'||p_event);
 RETURN o;
END $$;
CREATE FUNCTION app.orders_finish_return_refund(p_return uuid) RETURNS app.orders
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); r app.returns; o app.orders; refunded bigint;
BEGIN
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return;
 SELECT * INTO STRICT o FROM app.orders WHERE tenant_id=t AND id=r.order_id FOR UPDATE;
 IF r.status<>'refund_pending' OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded')
  OR r.refund_minor<=0 OR r.refund_minor>o.total_minor-o.refunded_minor THEN
  RAISE EXCEPTION 'FULFILLMENT_REFUND_BOUND' USING ERRCODE='23514'; END IF;
 refunded:=o.refunded_minor+r.refund_minor;
 UPDATE app.orders SET refunded_minor=refunded,state=CASE WHEN refunded=total_minor THEN 'refunded' ELSE 'partially_refunded' END,
  version=version+1,updated_at=clock_timestamp() WHERE tenant_id=t AND id=o.id RETURNING * INTO o;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.'||o.state,jsonb_build_object('order_id',o.id,'return_id',r.id,'refund_minor',r.refund_minor,'version',o.version),
  'orders.events','return-refund:'||r.id::text);
 RETURN o;
END $$;
-- Inventory owns restoration; caller supplies only the immutable return ID.
CREATE FUNCTION app.inventory_restore_return(p_return uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.inventory_context(); r app.returns; line record; s app.stock_levels;
BEGIN
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF NOT r.sealed OR r.status<>'received' OR NOT r.restocked THEN
  RAISE EXCEPTION 'FULFILLMENT_RESTOCK_STATE' USING ERRCODE='55000'; END IF;
 FOR line IN SELECT ol.sku_id,sum(rl.quantity)::bigint AS quantity
  FROM app.return_lines rl JOIN app.order_lines ol ON ol.tenant_id=rl.tenant_id AND ol.id=rl.order_line_id
  WHERE rl.tenant_id=t AND rl.return_id=p_return GROUP BY ol.sku_id ORDER BY ol.sku_id LOOP
  SELECT * INTO STRICT s FROM app.stock_levels WHERE tenant_id=t AND sku_id=line.sku_id FOR UPDATE;
  -- RETURN_RESTORE_ONCE: stock movement identity survives idempotency/outbox purge.
  IF EXISTS(SELECT FROM app.inventory_movements WHERE tenant_id=t AND owner_type='return' AND owner_id=p_return AND sku_id=line.sku_id) THEN
   RAISE EXCEPTION 'FULFILLMENT_RESTOCK_DUPLICATE' USING ERRCODE='23505'; END IF;
  UPDATE app.stock_levels SET on_hand=on_hand+line.quantity WHERE tenant_id=t AND sku_id=line.sku_id RETURNING * INTO s;
  INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,owner_type,owner_id,kind,quantity,
   on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
  VALUES(t,s.warehouse_id,s.sku_id,'return',p_return,'inbound',line.quantity,line.quantity,0,s.on_hand,s.reserved,r.reason);
 END LOOP;
END $$;
-- Exact integer intermediates avoid bigint-product overflow; no money is stored
-- as numeric and no rounding/floating conversion participates in allocation.
CREATE FUNCTION app.fulfillment_allocate(p_amount bigint,p_weights bigint[]) RETURNS bigint[]
LANGUAGE plpgsql IMMUTABLE SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE total numeric; left_units bigint; parts bigint[]; i integer; chosen integer; rems numeric[];
BEGIN
 IF p_amount IS NULL OR p_amount<0 OR p_weights IS NULL OR cardinality(p_weights)=0
  OR EXISTS(SELECT FROM unnest(p_weights) w WHERE w IS NULL OR w<0) THEN
  RAISE EXCEPTION 'FULFILLMENT_ALLOCATION_INPUT' USING ERRCODE='22023'; END IF;
 SELECT sum(w)::numeric INTO total FROM unnest(p_weights) w;
 IF total=0 THEN RAISE EXCEPTION 'FULFILLMENT_ALLOCATION_WEIGHTS' USING ERRCODE='22023'; END IF;
 parts:=ARRAY[]::bigint[];rems:=ARRAY[]::numeric[];left_units:=p_amount;
 FOR i IN 1..cardinality(p_weights) LOOP
  parts:=array_append(parts,div(p_amount::numeric*p_weights[i],total)::bigint);
  rems:=array_append(rems,mod(p_amount::numeric*p_weights[i],total));left_units:=left_units-parts[i];
 END LOOP;
 WHILE left_units>0 LOOP
  SELECT x INTO chosen FROM generate_series(1,cardinality(p_weights)) x ORDER BY rems[x] DESC,x LIMIT 1;
  parts[chosen]:=parts[chosen]+1;rems[chosen]:=-1;left_units:=left_units-1;
 END LOOP;
 RETURN parts;
END $$;
CREATE FUNCTION app.fulfillment_return_json(p_id uuid) RETURNS jsonb
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT (to_jsonb(r)-ARRAY['tenant_id','creation_key','request_hash','receive_key','receive_hash','refund_hash','sealed','creation_response','receive_response'])
  ||jsonb_build_object('lines',coalesce((SELECT jsonb_agg(jsonb_build_object('id',l.id,'order_line_id',l.order_line_id,
    'shipment_line_id',l.shipment_line_id,'quantity',l.quantity) ORDER BY l.order_line_id,l.shipment_line_id,l.id)
    FROM app.return_lines l WHERE l.tenant_id=r.tenant_id AND l.return_id=r.id),'[]'::jsonb))
 FROM app.returns r WHERE r.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND r.id=p_id
$$;
CREATE FUNCTION app.fulfillment_shipment_json(p_id uuid) RETURNS jsonb
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT (to_jsonb(s)-ARRAY['tenant_id','creation_key','request_hash','sealed'])||jsonb_build_object('lines',coalesce((
  SELECT jsonb_agg(jsonb_build_object('id',l.id,'order_line_id',l.order_line_id,'quantity',l.quantity) ORDER BY l.order_line_id,l.id)
  FROM app.shipment_lines l WHERE l.tenant_id=s.tenant_id AND l.shipment_id=s.id),'[]'::jsonb))
 FROM app.shipments s WHERE s.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND s.id=p_id
$$;
CREATE FUNCTION app.fulfillment_header_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF current_user<>'app_fulfillment_executor' OR TG_OP='DELETE' THEN
  RAISE EXCEPTION 'FULFILLMENT_CONTROLLED_COMMAND_REQUIRED' USING ERRCODE='55000'; END IF;
 IF TG_OP='INSERT' THEN RETURN NEW; END IF;
 IF OLD.sealed AND (NOT NEW.sealed OR
  (to_jsonb(NEW)-ARRAY['creation_response','receive_response','order_version','status','restocked','received_at','receive_key','receive_hash','refund_key','refund_hash',
    'refund_expected_version','provider_ref','payment_version','income_account_id','tax_account_id','shipping_account_id','cash_account_id','ledger_transaction_id','refunded_at'])
  IS DISTINCT FROM
  (to_jsonb(OLD)-ARRAY['creation_response','receive_response','order_version','status','restocked','received_at','receive_key','receive_hash','refund_key','refund_hash',
    'refund_expected_version','provider_ref','payment_version','income_account_id','tax_account_id','shipping_account_id','cash_account_id','ledger_transaction_id','refunded_at'])) THEN
  RAISE EXCEPTION 'FULFILLMENT_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF TG_TABLE_NAME='returns' THEN
  IF OLD.creation_response IS NOT NULL AND NEW.creation_response IS DISTINCT FROM OLD.creation_response
   OR OLD.receive_response IS NOT NULL AND NEW.receive_response IS DISTINCT FROM OLD.receive_response THEN
   RAISE EXCEPTION 'FULFILLMENT_RECEIPT_IMMUTABLE' USING ERRCODE='55000'; END IF;
  -- Compile return-only fields only for return records; shipments use this guard too.
  IF OLD.status IS DISTINCT FROM NEW.status AND NOT(
   OLD.status='requested' AND NEW.status='received' OR OLD.status='received' AND NEW.status='refund_pending'
   OR OLD.status='refund_pending' AND NEW.status='refunded'
   OR OLD.status='received' AND NEW.status='completed' AND NEW.refund_minor=0) THEN
   RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shipment_header_immutable BEFORE INSERT OR UPDATE OR DELETE ON app.shipments
 FOR EACH ROW EXECUTE FUNCTION app.fulfillment_header_guard();
CREATE TRIGGER return_header_immutable BEFORE INSERT OR UPDATE OR DELETE ON app.returns
 FOR EACH ROW EXECUTE FUNCTION app.fulfillment_header_guard();
CREATE FUNCTION app.fulfillment_line_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; l app.order_lines; s app.shipments; r app.returns; sent numeric; cancelled numeric; returned numeric; source_qty bigint;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'FULFILLMENT_LINE_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM t THEN RAISE EXCEPTION 'FULFILLMENT_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 o:=app.orders_fulfillment_fence(NEW.order_id,NULL);
 IF NOT o.sealed OR (o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded')
  AND NOT(TG_TABLE_NAME='return_lines' AND o.state='refunded')) THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 SELECT * INTO l FROM app.order_lines WHERE tenant_id=t AND order_id=NEW.order_id AND id=NEW.order_line_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_ORDER_LINE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF NOT EXISTS(SELECT FROM app.inventory_reservations ir WHERE ir.tenant_id=t AND ir.id=l.reservation_id
  AND ir.owner_type='order' AND ir.owner_id=NEW.order_id AND ir.sku_id=l.sku_id AND ir.quantity=l.quantity AND ir.state='committed') THEN
  RAISE EXCEPTION 'FULFILLMENT_COMMITTED_RESERVATION_REQUIRED' USING ERRCODE='42501'; END IF;
 SELECT coalesce(sum(quantity),0) INTO sent FROM app.shipment_lines WHERE tenant_id=t AND order_line_id=l.id;
 SELECT coalesce(sum(rl.quantity),0) INTO cancelled FROM app.return_lines rl JOIN app.returns h ON h.tenant_id=rl.tenant_id AND h.id=rl.return_id
  WHERE rl.tenant_id=t AND rl.order_line_id=l.id AND h.kind='unshipped_cancel';
 IF TG_TABLE_NAME='shipment_lines' THEN
  SELECT * INTO STRICT s FROM app.shipments WHERE tenant_id=t AND id=NEW.shipment_id;
  IF s.sealed OR s.order_id<>NEW.order_id THEN RAISE EXCEPTION 'FULFILLMENT_LINE_PARENT_SEALED' USING ERRCODE='55000'; END IF;
  -- QUANTITY_ALLOCATION_BOUND: cancellation and shipment compete for identical units.
  IF sent+cancelled+NEW.quantity>l.quantity THEN RAISE EXCEPTION 'FULFILLMENT_OVERSHIP' USING ERRCODE='23514'; END IF;
 ELSE
  SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=NEW.return_id;
  IF r.sealed OR r.order_id<>NEW.order_id THEN RAISE EXCEPTION 'FULFILLMENT_LINE_PARENT_SEALED' USING ERRCODE='55000'; END IF;
  IF r.kind='unshipped_cancel' THEN
   IF NEW.shipment_line_id IS NOT NULL OR sent+cancelled+NEW.quantity>l.quantity THEN
    RAISE EXCEPTION 'FULFILLMENT_OVERCANCEL' USING ERRCODE='23514'; END IF;
  ELSE
   SELECT quantity INTO source_qty FROM app.shipment_lines WHERE tenant_id=t AND id=NEW.shipment_line_id AND order_line_id=l.id AND order_id=NEW.order_id;
   IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_SHIPMENT_LINE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
   SELECT coalesce(sum(quantity),0) INTO returned FROM app.return_lines WHERE tenant_id=t AND shipment_line_id=NEW.shipment_line_id;
   -- RETURN_QUANTITY_BOUND: each shipped source has a permanent quantity budget.
   IF returned+NEW.quantity>source_qty THEN RAISE EXCEPTION 'FULFILLMENT_OVERRETURN' USING ERRCODE='23514'; END IF;
  END IF;
  SELECT coalesce(sum(quantity),0) INTO returned FROM app.return_lines WHERE tenant_id=t AND order_line_id=l.id;
  IF returned+NEW.quantity>l.quantity THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_ORDER_BOUND' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shipment_line_bound BEFORE INSERT OR UPDATE OR DELETE ON app.shipment_lines
 FOR EACH ROW EXECUTE FUNCTION app.fulfillment_line_guard();
CREATE TRIGGER return_line_bound BEFORE INSERT OR UPDATE OR DELETE ON app.return_lines
 FOR EACH ROW EXECUTE FUNCTION app.fulfillment_line_guard();

CREATE FUNCTION app.fulfillment_create_shipment(p_order uuid,p_expected_version bigint,p_key text,p_carrier text,p_tracking text,p_lines jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; s app.shipments; h bytea; line record;
BEGIN
 IF p_order IS NULL OR p_expected_version IS NULL OR p_expected_version<=0 OR p_key IS NULL OR p_key !~ '^[!-~]{1,128}$'
  OR p_carrier IS NULL OR length(p_carrier) NOT BETWEEN 1 AND 128 OR btrim(p_carrier)='' OR p_tracking IS NULL OR length(p_tracking)>256
  OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR jsonb_array_length(p_lines) NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'FULFILLMENT_ARGUMENT' USING ERRCODE='22023'; END IF;
 h:=sha256(convert_to(jsonb_build_object('order_id',p_order,'expected_version',p_expected_version,'carrier',p_carrier,'tracking',p_tracking,'lines',p_lines)::text,'UTF8'));
 SELECT * INTO s FROM app.shipments WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF s.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_shipment_json(s.id);
 END IF;
 o:=app.orders_fulfillment_fence(p_order,NULL);
 -- A concurrent identical command may have appeared while waiting on Orders.
 SELECT * INTO s FROM app.shipments WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF s.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_shipment_json(s.id);
 END IF;
 IF o.version<>p_expected_version THEN RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 IF NOT o.sealed OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 INSERT INTO app.shipments(tenant_id,order_id,creation_key,request_hash,carrier,tracking_number)
 VALUES(t,p_order,p_key,h,p_carrier,p_tracking) RETURNING * INTO s;
 FOR line IN SELECT * FROM jsonb_to_recordset(p_lines) AS x(order_line_id uuid,quantity bigint) ORDER BY order_line_id LOOP
  IF line.order_line_id IS NULL OR line.quantity IS NULL OR line.quantity<=0 THEN
   RAISE EXCEPTION 'FULFILLMENT_LINE_ARGUMENT' USING ERRCODE='22023'; END IF;
  IF NOT EXISTS(SELECT FROM app.order_lines l JOIN app.inventory_reservations ir ON ir.tenant_id=l.tenant_id AND ir.id=l.reservation_id
   WHERE l.tenant_id=t AND l.order_id=p_order AND l.id=line.order_line_id AND ir.state='committed'
    AND ir.owner_type='order' AND ir.owner_id=p_order AND ir.sku_id=l.sku_id AND ir.quantity=l.quantity) THEN
   RAISE EXCEPTION 'FULFILLMENT_COMMITTED_OWNER_REQUIRED' USING ERRCODE='42501'; END IF;
  INSERT INTO app.shipment_lines(tenant_id,shipment_id,order_id,order_line_id,quantity)
   VALUES(t,s.id,p_order,line.order_line_id,line.quantity);
 END LOOP;
 o:=app.orders_fulfillment_advance(p_order,'shipment:'||s.id::text);
 UPDATE app.shipments SET sealed=true,order_version=o.version WHERE tenant_id=t AND id=s.id;
 RETURN app.fulfillment_shipment_json(s.id);
END $$;
CREATE FUNCTION app.returns_create(p_order uuid,p_expected_version bigint,p_key text,p_kind text,p_reason text,p_lines jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; r app.returns; h bytea; line record; ol app.order_lines;
 ids uuid[]; weights bigint[]; income_parts bigint[]; tax_parts bigint[]:=ARRAY[]::bigint[]; shipping_parts bigint[];
 original_income bigint[]; original_shipping bigint[]; index_v integer; income bigint;
 frozen record; legacy numeric; has_legacy boolean; credit bigint;
 prior numeric; n bigint; tax bigint; ship bigint; total_income bigint;total_tax bigint;total_shipping bigint;
BEGIN
 IF p_order IS NULL OR p_expected_version IS NULL OR p_expected_version<=0 OR p_key IS NULL OR p_key !~ '^[!-~]{1,128}$'
  OR p_kind IS NULL OR p_kind NOT IN('shipment_return','unshipped_cancel') OR p_reason IS NULL OR length(p_reason) NOT BETWEEN 1 AND 1000 OR btrim(p_reason)=''
  OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR jsonb_array_length(p_lines) NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'FULFILLMENT_ARGUMENT' USING ERRCODE='22023'; END IF;
 h:=sha256(convert_to(jsonb_build_object('order_id',p_order,'expected_version',p_expected_version,'kind',p_kind,'reason',p_reason,'lines',p_lines)::text,'UTF8'));
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r.creation_response;
 END IF;
 o:=app.orders_fulfillment_fence(p_order,NULL);
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r.creation_response;
 END IF;
 IF o.version<>p_expected_version THEN RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 IF NOT o.sealed OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded','refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 income:=o.items_net_minor+o.adjustment_minor;
 IF income<0 THEN RAISE EXCEPTION 'FULFILLMENT_NEGATIVE_ADJUSTED_INCOME' USING ERRCODE='22023'; END IF;
 SELECT array_agg(id ORDER BY id),array_agg(line_net_minor ORDER BY id) INTO ids,weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 IF NOT EXISTS(SELECT FROM unnest(weights) x WHERE x>0) THEN
  SELECT array_agg(quantity ORDER BY id) INTO weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 END IF;
 income_parts:=app.fulfillment_allocate(income,weights);
 SELECT array_agg(quantity ORDER BY id) INTO weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 shipping_parts:=app.fulfillment_allocate(o.shipping_minor,weights);
 original_income:=income_parts; original_shipping:=shipping_parts;
 -- LEGACY_REFUND_CREDIT: direct Orders refunds have no component allocation.
 -- Preserve every frozen receipt, then attribute that credit only to remaining
 -- capacities in UUID line order, income/tax/shipping order. A saturated prefix
 -- cannot receive another allocation, so attribution stays monotone even when a
 -- legacy refund arrives between completed RMAs. Never reweight original caps.
 SELECT o.refunded_minor-coalesce(sum(rr.refund_minor),0) INTO legacy
 FROM app.returns rr WHERE rr.tenant_id=t AND rr.order_id=p_order AND rr.status='refunded';
 IF legacy<0 THEN RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
 legacy:=0; -- INT39 fault probe: ignore legacy credit.
 has_legacy:=legacy>0;
 FOR ol IN SELECT * FROM app.order_lines WHERE tenant_id=t AND order_id=p_order ORDER BY id LOOP
  index_v:=array_position(ids,ol.id);
  SELECT coalesce(sum(rl.income_minor),0) AS income,coalesce(sum(rl.tax_minor),0) AS tax,
   coalesce(sum(rl.shipping_minor),0) AS shipping INTO frozen
  FROM app.return_lines rl WHERE rl.tenant_id=t AND rl.order_line_id=ol.id;
  IF frozen.income>income_parts[index_v] OR frozen.tax>ol.line_tax_minor OR frozen.shipping>shipping_parts[index_v] THEN
   RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
  income_parts[index_v]:=(income_parts[index_v]::numeric-frozen.income)::bigint;
  tax_parts:=array_append(tax_parts,(ol.line_tax_minor::numeric-frozen.tax)::bigint);
  shipping_parts[index_v]:=(shipping_parts[index_v]::numeric-frozen.shipping)::bigint;
  credit:=least(legacy,income_parts[index_v])::bigint;
  income_parts[index_v]:=income_parts[index_v]-credit; legacy:=legacy-credit;
  credit:=least(legacy,tax_parts[index_v])::bigint;
  tax_parts[index_v]:=tax_parts[index_v]-credit; legacy:=legacy-credit;
  credit:=least(legacy,shipping_parts[index_v])::bigint;
  shipping_parts[index_v]:=shipping_parts[index_v]-credit; legacy:=legacy-credit;
 END LOOP;
 IF legacy<>0 THEN RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
 INSERT INTO app.returns(tenant_id,order_id,kind,creation_key,request_hash,reason,currency,status,restocked,received_at)
 VALUES(t,p_order,p_kind,p_key,h,p_reason,o.currency,CASE p_kind WHEN 'unshipped_cancel' THEN 'received' ELSE 'requested' END,
  p_kind='unshipped_cancel',CASE WHEN p_kind='unshipped_cancel' THEN clock_timestamp() END) RETURNING * INTO r;
 FOR line IN SELECT * FROM jsonb_to_recordset(p_lines) AS x(order_line_id uuid,shipment_line_id uuid,quantity bigint)
  ORDER BY order_line_id,shipment_line_id LOOP
  IF line.order_line_id IS NULL OR line.quantity IS NULL OR line.quantity<=0 THEN
   RAISE EXCEPTION 'FULFILLMENT_LINE_ARGUMENT' USING ERRCODE='22023'; END IF;
  SELECT * INTO ol FROM app.order_lines WHERE tenant_id=t AND order_id=p_order AND id=line.order_line_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_ORDER_LINE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
  index_v:=array_position(ids,ol.id);
  SELECT coalesce(sum(quantity),0) INTO prior FROM app.return_lines WHERE tenant_id=t AND order_line_id=ol.id;
  IF prior+line.quantity>ol.quantity THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_ORDER_BOUND' USING ERRCODE='23514'; END IF;
  IF has_legacy THEN
   -- Remaining units share only unfrozen, uncredited capacities. The final
   -- remaining unit takes every component's dust; numeric products are exact.
   n:=div(income_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
   tax:=div(tax_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
   ship:=div(shipping_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
  ELSE
   -- CUMULATIVE_ENTITLEMENT: retain the original no-legacy per-unit rounding.
   n:=(div(original_income[index_v]::numeric*(prior+line.quantity),ol.quantity)-div(original_income[index_v]::numeric*prior,ol.quantity))::bigint;
   tax:=(div(ol.line_tax_minor::numeric*(prior+line.quantity),ol.quantity)-div(ol.line_tax_minor::numeric*prior,ol.quantity))::bigint;
   ship:=(div(original_shipping[index_v]::numeric*(prior+line.quantity),ol.quantity)-div(original_shipping[index_v]::numeric*prior,ol.quantity))::bigint;
  END IF;
  IF n>income_parts[index_v] OR tax>tax_parts[index_v] OR ship>shipping_parts[index_v] THEN
   RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
  income_parts[index_v]:=income_parts[index_v]-n;
  tax_parts[index_v]:=tax_parts[index_v]-tax;
  shipping_parts[index_v]:=shipping_parts[index_v]-ship;
  INSERT INTO app.return_lines(tenant_id,return_id,order_id,order_line_id,shipment_line_id,quantity,income_minor,tax_minor,shipping_minor)
   VALUES(t,r.id,p_order,ol.id,line.shipment_line_id,line.quantity,n,tax,ship);
 END LOOP;
 SELECT sum(income_minor)::bigint,sum(tax_minor)::bigint,sum(shipping_minor)::bigint INTO total_income,total_tax,total_shipping
 FROM app.return_lines WHERE tenant_id=t AND return_id=r.id;
 UPDATE app.returns SET income_minor=total_income,tax_minor=total_tax,shipping_minor=total_shipping,
  refund_minor=total_income+total_tax+total_shipping,sealed=true WHERE tenant_id=t AND id=r.id;
 IF p_kind='unshipped_cancel' THEN PERFORM app.inventory_restore_return(r.id); END IF;
 IF p_kind='unshipped_cancel' AND total_income+total_tax+total_shipping=0 THEN
  UPDATE app.returns SET status='completed' WHERE tenant_id=t AND id=r.id;
 END IF;
 o:=app.orders_fulfillment_advance(p_order,'return:'||r.id::text);
 UPDATE app.returns SET order_version=o.version WHERE tenant_id=t AND id=r.id;
 UPDATE app.returns SET creation_response=app.fulfillment_return_json(r.id) WHERE tenant_id=t AND id=r.id RETURNING * INTO r;
 RETURN r.creation_response;
END $$;
CREATE FUNCTION app.returns_receive(p_return uuid,p_expected_version bigint,p_key text,p_restock boolean)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); r app.returns; o app.orders; h bytea;
BEGIN
 IF p_return IS NULL OR p_expected_version IS NULL OR p_expected_version<=0 OR p_key IS NULL OR p_key !~ '^[!-~]{1,128}$' OR p_restock IS NULL THEN
  RAISE EXCEPTION 'FULFILLMENT_ARGUMENT' USING ERRCODE='22023'; END IF;
 h:=sha256(convert_to(jsonb_build_object('return_id',p_return,'expected_version',p_expected_version,'restock',p_restock)::text,'UTF8'));
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.receive_key IS NOT NULL THEN
  IF r.receive_key<>p_key OR r.receive_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r.receive_response;
 END IF;
 o:=app.orders_fulfillment_fence(r.order_id,NULL);
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return FOR UPDATE;
 IF r.receive_key IS NOT NULL THEN
  IF r.receive_key<>p_key OR r.receive_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r.receive_response;
 END IF;
 IF o.version<>p_expected_version THEN RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 IF NOT o.sealed OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded','refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 IF r.kind<>'shipment_return' OR r.status<>'requested' OR NOT r.sealed THEN
  RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 UPDATE app.returns SET status='received',received_at=clock_timestamp(),restocked=p_restock,receive_key=p_key,receive_hash=h
 WHERE tenant_id=t AND id=p_return;
 IF p_restock THEN PERFORM app.inventory_restore_return(p_return); END IF;
 IF r.refund_minor=0 THEN UPDATE app.returns SET status='completed' WHERE tenant_id=t AND id=p_return; END IF;
 o:=app.orders_fulfillment_advance(r.order_id,'receive:'||r.id::text);
 UPDATE app.returns SET order_version=o.version WHERE tenant_id=t AND id=p_return;
 UPDATE app.returns SET receive_response=app.fulfillment_return_json(r.id) WHERE tenant_id=t AND id=p_return RETURNING * INTO r;
 RETURN r.receive_response;
END $$;
CREATE FUNCTION app.returns_refund_snapshot(p_return uuid,p_key text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); r app.returns;
BEGIN
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.refund_key IS NOT NULL AND r.refund_key IS DISTINCT FROM p_key THEN
  RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
 RETURN app.fulfillment_return_json(r.id);
END $$;
CREATE FUNCTION app.returns_prepare_refund(p_return uuid,p_expected_version bigint,p_key text,p_provider_ref text,p_payment_version bigint,
 p_income_account uuid,p_tax_account uuid,p_shipping_account uuid,p_cash_account uuid)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); r app.returns; o app.orders; h bytea; pending numeric;
BEGIN
 IF p_return IS NULL OR p_expected_version IS NULL OR p_expected_version<=0 OR p_key IS NULL OR p_key !~ '^[!-~]{1,128}$'
  OR p_provider_ref IS NULL OR length(p_provider_ref) NOT BETWEEN 1 AND 256 OR btrim(p_provider_ref)='' OR p_payment_version IS NULL OR p_payment_version<=0
  OR p_income_account IS NULL OR p_tax_account IS NULL OR p_shipping_account IS NULL OR p_cash_account IS NULL
  OR cardinality(ARRAY(SELECT DISTINCT a FROM unnest(ARRAY[p_income_account,p_tax_account,p_shipping_account,p_cash_account]) a))<>4 THEN
  RAISE EXCEPTION 'FULFILLMENT_ARGUMENT' USING ERRCODE='22023'; END IF;
 h:=sha256(convert_to(jsonb_build_object('return_id',p_return,'expected_version',p_expected_version,'provider_ref',p_provider_ref,'payment_version',p_payment_version,
  'income_account_id',p_income_account,'tax_account_id',p_tax_account,'shipping_account_id',p_shipping_account,'cash_account_id',p_cash_account)::text,'UTF8'));
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.refund_key IS NOT NULL THEN
  IF r.refund_key<>p_key OR r.refund_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_return_json(r.id);
 END IF;
 o:=app.orders_fulfillment_fence(r.order_id,NULL);
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return FOR UPDATE;
 IF r.refund_key IS NOT NULL THEN
  IF r.refund_key<>p_key OR r.refund_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_return_json(r.id);
 END IF;
 IF o.version<>p_expected_version THEN RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 IF NOT o.sealed OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 IF r.status<>'received' OR r.refund_minor<=0 THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 SELECT coalesce(sum(refund_minor),0) INTO pending FROM app.returns WHERE tenant_id=t AND order_id=r.order_id AND status='refund_pending';
 -- REFUND_PENDING_BOUND: durable reservations bound independent PSP operations.
 IF o.refunded_minor+pending+r.refund_minor>o.total_minor THEN
  RAISE EXCEPTION 'FULFILLMENT_REFUND_BOUND' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT FROM app.ledger_accounts WHERE tenant_id=t AND id=p_income_account AND currency=r.currency AND status='active' AND account_type='revenue')
  OR NOT EXISTS(SELECT FROM app.ledger_accounts WHERE tenant_id=t AND id=p_tax_account AND currency=r.currency AND status='active' AND account_type='liability')
  OR NOT EXISTS(SELECT FROM app.ledger_accounts WHERE tenant_id=t AND id=p_shipping_account AND currency=r.currency AND status='active' AND account_type='revenue')
  OR NOT EXISTS(SELECT FROM app.ledger_accounts WHERE tenant_id=t AND id=p_cash_account AND currency=r.currency AND status='active' AND account_type='asset') THEN
  RAISE EXCEPTION 'FULFILLMENT_LEDGER_ACCOUNT_CONFIG' USING ERRCODE='23514'; END IF;
 UPDATE app.returns SET status='refund_pending',refund_key=p_key,refund_hash=h,refund_expected_version=p_expected_version,
  provider_ref=p_provider_ref,payment_version=p_payment_version,income_account_id=p_income_account,tax_account_id=p_tax_account,
  shipping_account_id=p_shipping_account,cash_account_id=p_cash_account WHERE tenant_id=t AND id=p_return;
 o:=app.orders_fulfillment_advance(r.order_id,'refund-prepare:'||r.id::text);
 UPDATE app.returns SET order_version=o.version WHERE tenant_id=t AND id=p_return;
 RETURN app.fulfillment_return_json(r.id);
END $$;
CREATE FUNCTION app.returns_complete_refund(p_return uuid,p_key text,p_provider_ref text,p_ledger_transaction uuid)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); r app.returns; o app.orders; j app.ledger_transactions;
 accounts uuid[]:=ARRAY[]::uuid[];sides text[]:=ARRAY[]::text[];amounts bigint[]:=ARRAY[]::bigint[];
 got_accounts uuid[];got_sides text[];got_amounts bigint[];
BEGIN
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_key IS NULL OR p_provider_ref IS NULL OR p_ledger_transaction IS NULL OR r.refund_key IS DISTINCT FROM p_key OR r.provider_ref IS DISTINCT FROM p_provider_ref THEN
  RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
 IF r.status='refunded' THEN
  IF r.ledger_transaction_id<>p_ledger_transaction THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_return_json(r.id);
 END IF;
 o:=app.orders_fulfillment_fence(r.order_id,NULL);
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return FOR UPDATE;
 IF r.status='refunded' THEN
  IF r.ledger_transaction_id<>p_ledger_transaction THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN app.fulfillment_return_json(r.id);
 END IF;
 IF r.status<>'refund_pending' THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 IF r.income_minor>0 THEN accounts:=array_append(accounts,r.income_account_id);sides:=array_append(sides,'debit');amounts:=array_append(amounts,r.income_minor); END IF;
 IF r.tax_minor>0 THEN accounts:=array_append(accounts,r.tax_account_id);sides:=array_append(sides,'debit');amounts:=array_append(amounts,r.tax_minor); END IF;
 IF r.shipping_minor>0 THEN accounts:=array_append(accounts,r.shipping_account_id);sides:=array_append(sides,'debit');amounts:=array_append(amounts,r.shipping_minor); END IF;
 accounts:=array_append(accounts,r.cash_account_id);sides:=array_append(sides,'credit');amounts:=array_append(amounts,r.refund_minor);
 SELECT * INTO j FROM app.ledger_transactions WHERE tenant_id=t AND id=p_ledger_transaction;
 SELECT array_agg(account_id ORDER BY line_no),array_agg(side ORDER BY line_no),array_agg(amount ORDER BY line_no)
 INTO got_accounts,got_sides,got_amounts FROM app.ledger_entries WHERE tenant_id=t AND transaction_id=p_ledger_transaction;
 -- REFUND_LEDGER_RECEIPT: Ledger.Post alone owns writes; completion proves exact journal identity/content.
 IF j.id IS NULL OR j.status<>'posted' OR j.currency<>r.currency OR j.source_type<>'return' OR j.source_id<>r.id::text OR j.event<>'refund'
  OR got_accounts IS DISTINCT FROM accounts OR got_sides IS DISTINCT FROM sides OR got_amounts IS DISTINCT FROM amounts THEN
  RAISE EXCEPTION 'FULFILLMENT_LEDGER_RECEIPT' USING ERRCODE='23514'; END IF;
 o:=app.orders_finish_return_refund(r.id);
 UPDATE app.returns SET status='refunded',ledger_transaction_id=p_ledger_transaction,refunded_at=clock_timestamp(),order_version=o.version
 WHERE tenant_id=t AND id=r.id;
 RETURN app.fulfillment_return_json(r.id);
END $$;

CREATE OR REPLACE FUNCTION app.orders_transition(p_id uuid,p_target text,p_event_key text,p_refund_minor bigint DEFAULT 0)
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
 IF p_target IN('partially_refunded','refunded') AND EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND order_id=p_id AND status IN('requested','received','refund_pending')) THEN
  RAISE EXCEPTION 'FULFILLMENT_PENDING_REFUND_COMMAND_REQUIRED' USING ERRCODE='55000'; END IF;
 IF p_target='fulfilled' AND EXISTS(SELECT FROM app.order_lines ol_check WHERE ol_check.tenant_id=t AND ol_check.order_id=p_id AND
  coalesce((SELECT sum(quantity) FROM app.shipment_lines WHERE tenant_id=t AND order_line_id=ol_check.id),0)<>ol_check.quantity) THEN
  RAISE EXCEPTION 'FULFILLMENT_SHIPMENT_COMMAND_REQUIRED' USING ERRCODE='55000'; END IF;
 IF p_target='partially_fulfilled' AND NOT EXISTS(SELECT FROM app.shipment_lines WHERE tenant_id=t AND order_id=p_id) THEN
  RAISE EXCEPTION 'FULFILLMENT_SHIPMENT_COMMAND_REQUIRED' USING ERRCODE='55000'; END IF;
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

GRANT SELECT ON app.inventory_reservations TO app_fulfillment_executor;
CREATE POLICY fulfillment_reservation_read ON app.inventory_reservations FOR SELECT TO app_fulfillment_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT CREATE ON SCHEMA app TO app_fulfillment_executor,app_orders_executor,app_inventory_executor;
DO $owners$ DECLARE f record; role_name text; BEGIN
 FOR f IN SELECT p.oid::regprocedure AS identity,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND (starts_with(p.proname,'fulfillment_') OR starts_with(p.proname,'returns_')
   OR p.proname IN('orders_fulfillment_fence','orders_fulfillment_advance','orders_finish_return_refund','inventory_restore_return')) LOOP
  role_name:=CASE WHEN starts_with(f.proname,'orders_') THEN 'app_orders_executor'
   WHEN starts_with(f.proname,'inventory_') THEN 'app_inventory_executor' ELSE 'app_fulfillment_executor' END;
  EXECUTE format('ALTER FUNCTION %s OWNER TO %I',f.identity,role_name);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC,app_runtime,app_returns_service',f.identity);
 END LOOP;
END $owners$;
REVOKE CREATE ON SCHEMA app FROM app_fulfillment_executor,app_orders_executor,app_inventory_executor;
GRANT EXECUTE ON FUNCTION app.orders_context(),app.orders_fulfillment_fence(uuid,bigint),app.orders_fulfillment_advance(uuid,text),
 app.orders_finish_return_refund(uuid),app.inventory_restore_return(uuid) TO app_fulfillment_executor;
GRANT EXECUTE ON FUNCTION app.fulfillment_create_shipment(uuid,bigint,text,text,text,jsonb),app.returns_create(uuid,bigint,text,text,text,jsonb),
 app.returns_receive(uuid,bigint,text,boolean),app.returns_refund_snapshot(uuid,text) TO app_runtime;
GRANT EXECUTE ON FUNCTION app.returns_refund_snapshot(uuid,text),
 app.returns_prepare_refund(uuid,bigint,text,text,bigint,uuid,uuid,uuid,uuid),app.returns_complete_refund(uuid,text,text,uuid),
 app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) TO app_returns_service;
