-- Responsibility: fulfillment/RMA/financial invariants against real PostgreSQL.
-- Dependencies: catalog, paid Orders, Inventory and public Ledger commands.
-- Called by check_pg.sh; all witnesses are tenant-scoped and roll back.
-- Invariants: paid stock is committed once; allocations and financial receipts
-- cannot overrun immutable order quantities, replay, or cross tenant boundaries.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('fulfillment-A','USD') RETURNING id AS t \gset
INSERT INTO app.tenants(name,settlement_currency) VALUES('fulfillment-B','USD') RETURNING id AS other_t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','fulfillment') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'product','fulfillment-sku','default') RETURNING id AS sku \gset
SELECT set_config('test.fulfillment_t',:'t',true),set_config('test.fulfillment_other_t',:'other_t',true),
 set_config('test.fulfillment_sku',:'sku',true),set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',20,'fulfillment receipt',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $fulfillment$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid; sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid:=uuidv7(); lid uuid:=uuidv7(); reservation app.inventory_reservations; ord app.orders;
 shipped jsonb; cancelled jsonb; returned jsonb; received jsonb; prepared jsonb; completed jsonb;
 sid uuid; cancel_id uuid; return_id uuid; stock bigint; version_before bigint;create_version bigint;receive_version bigint;
 income uuid:=uuidv7(); tax uuid:=uuidv7(); shipping uuid:=uuidv7(); cash uuid:=uuidv7(); journal uuid;
BEGIN
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'fulfillment-income','USD','revenue'),(t,tax,'fulfillment-tax','USD','liability'),
  (t,shipping,'fulfillment-shipping','USD','revenue'),(t,cash,'fulfillment-cash','USD','asset');
 reservation:=app.inventory_reserve(sku,4,'order',oid,'fulfillment-reserve',900);
 ord:=app.orders_create(oid,'fulfillment-order',sha256('fulfillment'::bytea),NULL,
  '{"email":"fulfillment@example.invalid"}','USD',40,'{"amount_minor":40,"currency":"USD","service":"fixture"}',
  jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',4,
   'unit_price_minor',100,'line_net_minor',400,'line_tax_minor',40,'line_total_minor',440,
   'reservation_id',reservation.id,'tax_snapshot','{}'::jsonb)),'[]');
 ord:=app.orders_transition(oid,'paid','fulfillment-pay',0);
 SELECT on_hand INTO stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
 IF stock<>16 OR (SELECT state FROM app.inventory_reservations WHERE tenant_id=t AND id=reservation.id)<>'committed' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE paid stock semantics changed';
 END IF;
 shipped:=app.fulfillment_create_shipment(oid,ord.version,'shipment-first','manual','TRACK-1',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)));
 IF shipped->>'status'<>'shipped' OR shipped->>'order_id'<>oid::text
  OR (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_fulfilled'
  OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>16 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE partial shipment state/stock';
 END IF;
 IF app.fulfillment_create_shipment(oid,ord.version,'shipment-first','manual','TRACK-1',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)))->>'id'<>shipped->>'id' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE shipment replay duplicated';
 END IF;
 SELECT id INTO sid FROM app.shipment_lines WHERE tenant_id=t AND shipment_id=(shipped->>'id')::uuid;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,ord.version,'shipment-over','manual','OVER',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',3)));
  RAISE EXCEPTION 'FULFILLMENT_GATE overship accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,1,'shipment-stale','manual','STALE',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
  RAISE EXCEPTION 'FULFILLMENT_GATE stale If-Match accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 cancelled:=app.returns_create(oid,ord.version,'cancel-one','unshipped_cancel','buyer cancelled remainder',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 cancel_id:=(cancelled->>'id')::uuid;
 IF cancelled->>'status'<>'received' OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE unshipped cancellation did not restore committed stock';
 END IF;
 PERFORM app.returns_create(oid,ord.version,'cancel-one','unshipped_cancel','buyer cancelled remainder',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 IF (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cancellation replay restocked twice';
 END IF;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,ord.version,'shipment-after-cancel','manual','OVER',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)));
  RAISE EXCEPTION 'FULFILLMENT_GATE shipment ignored cancellation reservation';
 EXCEPTION WHEN check_violation THEN NULL; END;
 create_version:=ord.version;
 returned:=app.returns_create(oid,ord.version,'return-one','shipment_return','received damaged',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
 return_id:=(returned->>'id')::uuid;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.returns_create(oid,ord.version,'return-over','shipment_return','too many',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',2)));
  RAISE EXCEPTION 'FULFILLMENT_GATE overreturn accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 receive_version:=ord.version;
 received:=app.returns_receive(return_id,ord.version,'receive-one',false);
 IF received->>'status'<>'received' OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE optional no-restock failed';
 END IF;
 PERFORM app.returns_receive(return_id,ord.version,'receive-one',false);
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.returns_prepare_refund(cancel_id,ord.version,'runtime-forged','captured-fixture',1,income,tax,shipping,cash);
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime forged PSP intent';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'runtime-forged','captured-fixture',uuidv7());
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime forged PSP completion';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 prepared:=app.returns_prepare_refund(cancel_id,ord.version,'refund-cancel','captured-fixture',1,income,tax,shipping,cash);
 IF prepared->>'status'<>'refund_pending' OR (prepared->>'refund_minor')::bigint<>120
  OR (prepared->>'income_minor')::bigint<>100 OR (prepared->>'tax_minor')::bigint<>10
  OR (prepared->>'shipping_minor')::bigint<>10 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE server refund allocation';
 END IF;
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'refund-cancel','captured-fixture',uuidv7());
  RAISE EXCEPTION 'FULFILLMENT_GATE unposted/foreign ledger accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 journal:=app.post_ledger(t,'USD','refund-probe',cancel_id::text,'wrong-receipt',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[1,1]::bigint[]);
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'refund-cancel','captured-fixture',journal);
  RAISE EXCEPTION 'FULFILLMENT_GATE posted wrong journal accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 journal:=app.post_ledger(t,'USD','return',cancel_id::text,'refund',ARRAY[income,tax,shipping,cash],
  ARRAY['debit','debit','debit','credit'],ARRAY[100,10,10,120]::bigint[]);
 completed:=app.returns_complete_refund(cancel_id,'refund-cancel','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF completed->>'status'<>'refunded' OR (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>120
  OR (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_refunded' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund not atomic with order state';
 END IF;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 PERFORM app.returns_complete_refund(cancel_id,'refund-cancel','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>120 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund replay duplicated amount';
 END IF;
 EXECUTE 'SET LOCAL ROLE app_runtime';
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 shipped:=app.fulfillment_create_shipment(oid,ord.version,'shipment-last','manual','TRACK-2',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 IF (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_refunded'
  OR (SELECT sum(quantity) FROM app.shipment_lines WHERE tenant_id=t AND order_line_id=lid)<>3 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund state blocked remaining shipment';
 END IF;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 prepared:=app.returns_prepare_refund(return_id,ord.version,'refund-return','captured-fixture',1,income,tax,shipping,cash);
 journal:=app.post_ledger(t,'USD','return',return_id::text,'refund',ARRAY[income,tax,shipping,cash],
  ARRAY['debit','debit','debit','credit'],ARRAY[100,10,10,120]::bigint[]);
 PERFORM app.returns_complete_refund(return_id,'refund-return','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>240 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cumulative refund amount';
 END IF;
 IF app.returns_create(oid,create_version,'return-one','shipment_return','received damaged',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1))) IS DISTINCT FROM returned THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE creation response drifted after refund'; END IF;
 IF app.returns_receive(return_id,receive_version,'receive-one',false) IS DISTINCT FROM received THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE receive response drifted after refund'; END IF;
 BEGIN
  UPDATE app.shipments SET tracking_number='forged' WHERE tenant_id=t;
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime mutated shipment';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  INSERT INTO app.return_lines(tenant_id,return_id,order_id,order_line_id,quantity,income_minor,tax_minor,shipping_minor)
  VALUES(t,cancel_id,oid,lid,1,100,10,10);
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime appended return line';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',current_setting('test.fulfillment_other_t'),true);
 IF EXISTS(SELECT FROM app.shipments) OR EXISTS(SELECT FROM app.returns) THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cross-tenant read';
 END IF;
 BEGIN
  PERFORM app.returns_receive(return_id,1,'foreign-return',true);
  RAISE EXCEPTION 'FULFILLMENT_GATE cross-tenant command';
 EXCEPTION WHEN no_data_found THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,1,'missing-context','manual','X','[]');
  RAISE EXCEPTION 'FULFILLMENT_GATE missing tenant context';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $fulfillment$;

-- Adversarial allocation: tiny components, unequal prices with a discount, and
-- a zero-price line. Each independently received unit has a monotone entitlement;
-- the final unit conserves every original financial component without drift.
RESET ROLE;
SELECT set_config('app.tenant_id',:'t',true);
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'product','fulfillment-sku-2','second') RETURNING id AS sku2 \gset
SELECT set_config('test.fulfillment_sku2',:'sku2',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku2',20,'allocation receipt',uuidv7(),'inbound');
RESET ROLE;
DO $allocation$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid; sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid; lid uuid; other_lid uuid;chosen_id uuid;sku2 uuid:=current_setting('test.fulfillment_sku2')::uuid;res app.inventory_reservations;res2 app.inventory_reservations;ord app.orders;r jsonb;
 income uuid:=uuidv7();tax uuid:=uuidv7();shipping uuid:=uuidv7();cash uuid:=uuidv7();journal uuid;
 keys uuid[];lines jsonb;accounts uuid[];sides text[];amounts bigint[];
 case_i int;unit_i int;n bigint;tx bigint;sh bigint;expected bigint[];actual bigint[];
BEGIN
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'allocation-income','USD','revenue'),(t,tax,'allocation-tax','USD','liability'),
  (t,shipping,'allocation-shipping','USD','revenue'),(t,cash,'allocation-cash','USD','asset');
 FOR case_i IN 1..3 LOOP
  oid:=uuidv7();lid:=uuidv7();other_lid:=uuidv7();
  IF case_i=1 THEN
   expected:=ARRAY[5,3,1];
   res:=app.inventory_reserve(sku,9,'order',oid,'tiny-reserve',900);
   lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',9,
    'unit_price_minor',1,'line_net_minor',9,'line_tax_minor',3,'line_total_minor',12,'reservation_id',res.id,'tax_snapshot','{}'::jsonb));
  ELSE
   expected:=CASE case_i WHEN 2 THEN ARRAY[25,8,5] ELSE ARRAY[0,0,4] END;
   IF case_i=2 THEN
    res:=app.inventory_reserve(sku,2,'order',oid,'allocation-reserve-2a',900);
    res2:=app.inventory_reserve(sku2,2,'order',oid,'allocation-reserve-2b',900);
    lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
     'unit_price_minor',5,'line_net_minor',10,'line_tax_minor',3,'line_total_minor',13,'reservation_id',res.id,'tax_snapshot','{}'::jsonb),
     jsonb_build_object('id',other_lid,'sku_id',sku2,'sku','fulfillment-sku-2','quantity',2,
     'unit_price_minor',9,'line_net_minor',18,'line_tax_minor',5,'line_total_minor',23,'reservation_id',res2.id,'tax_snapshot','{}'::jsonb));
   ELSE
    res:=app.inventory_reserve(sku,4,'order',oid,'allocation-reserve-3',900);
    lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',4,
     'unit_price_minor',0,'line_net_minor',0,'line_tax_minor',0,'line_total_minor',0,'reservation_id',res.id,'tax_snapshot','{}'::jsonb));
   END IF;
  END IF;
  ord:=app.orders_create(oid,'allocation-order-'||case_i,sha256(('allocation'||case_i)::bytea),NULL,
   '{"email":"allocation@example.invalid"}','USD',expected[3],jsonb_build_object('currency','USD','amount_minor',expected[3],'service','fixture'),
   lines,CASE WHEN case_i IN(1,2) THEN jsonb_build_array(jsonb_build_object('id',uuidv7(),'kind','discount','amount_minor',CASE case_i WHEN 1 THEN -4 ELSE -3 END,'reason','fixture discount')) ELSE '[]'::jsonb END);
  ord:=app.orders_transition(oid,'paid','allocation-paid-'||case_i,0);actual:=ARRAY[0,0,0]::bigint[];
  FOR unit_i IN 1..CASE case_i WHEN 1 THEN 9 ELSE 4 END LOOP
   chosen_id:=CASE WHEN case_i=2 AND unit_i%2=0 THEN other_lid ELSE lid END;
   r:=app.returns_create(oid,ord.version,'allocation-return-'||case_i||'-'||unit_i,'unshipped_cancel','unit entitlement',
    jsonb_build_array(jsonb_build_object('order_line_id',chosen_id,'quantity',1)));
   n:=(r->>'income_minor')::bigint;tx:=(r->>'tax_minor')::bigint;sh:=(r->>'shipping_minor')::bigint;
   IF n<0 OR tx<0 OR sh<0 THEN RAISE EXCEPTION 'FULFILLMENT_GATE nonmonotone tiny entitlement';END IF;
   actual:=ARRAY[actual[1]+n,actual[2]+tx,actual[3]+sh];
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
   IF (r->>'refund_minor')::bigint>0 THEN
    EXECUTE 'SET LOCAL ROLE app_returns_service';
    r:=app.returns_prepare_refund((r->>'id')::uuid,ord.version,'allocation-refund-'||case_i||'-'||unit_i,'capture-allocation',1,income,tax,shipping,cash);
    accounts:=ARRAY[]::uuid[];sides:=ARRAY[]::text[];amounts:=ARRAY[]::bigint[];
    IF n>0 THEN accounts:=array_append(accounts,income);sides:=array_append(sides,'debit');amounts:=array_append(amounts,n);END IF;
    IF tx>0 THEN accounts:=array_append(accounts,tax);sides:=array_append(sides,'debit');amounts:=array_append(amounts,tx);END IF;
    IF sh>0 THEN accounts:=array_append(accounts,shipping);sides:=array_append(sides,'debit');amounts:=array_append(amounts,sh);END IF;
    accounts:=array_append(accounts,cash);sides:=array_append(sides,'credit');amounts:=array_append(amounts,n+tx+sh);
    journal:=app.post_ledger(t,'USD','return',r->>'id','refund',accounts,sides,amounts);
    PERFORM app.returns_complete_refund((r->>'id')::uuid,'allocation-refund-'||case_i||'-'||unit_i,'capture-allocation',journal);
    EXECUTE 'SET LOCAL ROLE app_runtime';
   END IF;
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  END LOOP;
  IF actual<>expected OR ord.refunded_minor<>ord.total_minor OR ord.state<>'refunded' THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE financial conservation case % actual % expected %',case_i,actual,expected;END IF;
 END LOOP;
 -- Exact integer intermediates at 64-bit limits: division must never round up.
 EXECUTE 'SET LOCAL ROLE app_fulfillment_executor';
 IF app.fulfillment_allocate(9223372036854775807,ARRAY[9223372036854775806,1]::bigint[])<>
  ARRAY[9223372036854775806,1]::bigint[] THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE bigint allocation overflow or drift';END IF;
 EXECUTE 'SET LOCAL ROLE app_runtime';
END $allocation$;
ROLLBACK;
SELECT 'PASS: fulfillment allocation, cancellation restore, RMA, bounded refund and exact ledger receipts' AS result;
