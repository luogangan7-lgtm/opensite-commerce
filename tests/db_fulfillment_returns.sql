-- Responsibility: fulfillment/RMA/financial invariants against real PostgreSQL.
-- Dependencies: catalog, paid Orders, Inventory and public Ledger commands.
-- Called by check_pg.sh; all witnesses are tenant-scoped and roll back.
-- Invariants: paid stock is committed once; allocations and financial receipts
-- cannot overrun immutable order quantities, replay, or cross tenant boundaries.
\set ON_ERROR_STOP on
BEGIN;
-- A shared authority must exist before any functional proof can be accepted.
-- Catalog checks are followed by a same-OID intervention below, not name lint.
DO $budget_contract$
DECLARE command oid:=to_regprocedure('app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint)');
BEGIN
 IF command IS NULL THEN
  RAISE EXCEPTION 'SHARED_REFUND_BUDGET_AUTHORITY_MISSING';
 END IF;
 IF NOT EXISTS(SELECT FROM pg_proc p WHERE p.oid=command AND p.prorettype='app.orders'::regtype
  AND p.proowner='app_orders_executor'::regrole AND p.prosecdef
  AND p.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')
  AND p.proconfig=ARRAY['search_path=pg_catalog, app, pg_temp']) THEN
  RAISE EXCEPTION 'SHARED_REFUND_BUDGET_AUTHORITY_CATALOG';
 END IF;
 IF EXISTS(SELECT FROM pg_proc p CROSS JOIN LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
  WHERE p.oid=command AND a.privilege_type='EXECUTE'
  AND a.grantee NOT IN('app_orders_executor'::regrole,'app_fulfillment_executor'::regrole,'app_payments_service'::regrole))
  OR NOT has_function_privilege('app_fulfillment_executor',command,'EXECUTE')
  OR NOT has_function_privilege('app_payments_service',command,'EXECUTE')
  OR has_function_privilege('app_runtime',command,'EXECUTE')
  OR has_function_privilege('app_returns_service',command,'EXECUTE') THEN
  RAISE EXCEPTION 'SHARED_REFUND_BUDGET_AUTHORITY_ACL';
 END IF;
END $budget_contract$;
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
 reservation:=app.inventory_reserve(sku,4,'order',oid,'6e7d97c2-6b34-5b65-ba83-991e57707916',900);
 ord:=app.orders_create(oid,'8c29a995-921f-5199-bc9b-651183d6d7d2',sha256('fulfillment'::bytea),NULL,
  '{"email":"fulfillment@example.invalid"}','USD',40,'{"amount_minor":40,"currency":"USD","service":"fixture"}',
  jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',4,
   'unit_price_minor',100,'line_net_minor',400,'line_tax_minor',40,'line_total_minor',440,
   'reservation_id',reservation.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
 ord:=app.orders_transition(oid,'paid','fulfillment-pay',0);
 SELECT on_hand INTO stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
 IF stock<>16 OR (SELECT state FROM app.inventory_reservations WHERE tenant_id=t AND id=reservation.id)<>'committed' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE paid stock semantics changed';
 END IF;
 shipped:=app.fulfillment_create_shipment(oid,ord.version,'4ecca1b7-a168-5adc-981b-ee5ee966a8b0','manual','TRACK-1',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)));
 IF shipped->>'status'<>'shipped' OR shipped->>'order_id'<>oid::text
  OR (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_fulfilled'
  OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>16 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE partial shipment state/stock';
 END IF;
 IF app.fulfillment_create_shipment(oid,ord.version,'4ecca1b7-a168-5adc-981b-ee5ee966a8b0','manual','TRACK-1',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)))->>'id'<>shipped->>'id' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE shipment replay duplicated';
 END IF;
 SELECT id INTO sid FROM app.shipment_lines WHERE tenant_id=t AND shipment_id=(shipped->>'id')::uuid;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,ord.version,'e2377e1d-e2cc-5c50-ae0a-5baad9c8ba59','manual','OVER',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',3)));
  RAISE EXCEPTION 'FULFILLMENT_GATE overship accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,1,'276fd92a-64db-558c-85c8-41b549e90f1a','manual','STALE',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
  RAISE EXCEPTION 'FULFILLMENT_GATE stale If-Match accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 cancelled:=app.returns_create(oid,ord.version,'07297935-a8e1-5208-987c-84a38418e5da','unshipped_cancel','buyer cancelled remainder',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 cancel_id:=(cancelled->>'id')::uuid;
 IF cancelled->>'status'<>'received' OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE unshipped cancellation did not restore committed stock';
 END IF;
 PERFORM app.returns_create(oid,ord.version,'07297935-a8e1-5208-987c-84a38418e5da','unshipped_cancel','buyer cancelled remainder',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 IF (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cancellation replay restocked twice';
 END IF;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,ord.version,'7d78ae85-0524-564a-a12c-16d181204eff','manual','OVER',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)));
  RAISE EXCEPTION 'FULFILLMENT_GATE shipment ignored cancellation reservation';
 EXCEPTION WHEN check_violation THEN NULL; END;
 create_version:=ord.version;
 returned:=app.returns_create(oid,ord.version,'e66bbcd5-f69e-5c04-91a9-1f33e1af7582','shipment_return','received damaged',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
 return_id:=(returned->>'id')::uuid;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.returns_create(oid,ord.version,'12a8ee06-ea44-5686-b18a-0cdf7bf1011d','shipment_return','too many',
   jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',2)));
  RAISE EXCEPTION 'FULFILLMENT_GATE overreturn accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 receive_version:=ord.version;
 received:=app.returns_receive(return_id,ord.version,'35b5f949-2b9d-5cb2-9fc8-95a04269d498',false);
 IF received->>'status'<>'received' OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>17 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE optional no-restock failed';
 END IF;
 PERFORM app.returns_receive(return_id,ord.version,'35b5f949-2b9d-5cb2-9fc8-95a04269d498',false);
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 BEGIN
  PERFORM app.returns_prepare_refund(cancel_id,ord.version,'465e87be-41a0-514c-b177-f548a99ba71f','captured-fixture',1,income,tax,shipping,cash);
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime forged PSP intent';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'465e87be-41a0-514c-b177-f548a99ba71f','captured-fixture',uuidv7());
  RAISE EXCEPTION 'FULFILLMENT_GATE runtime forged PSP completion';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 prepared:=app.returns_prepare_refund(cancel_id,ord.version,'444723f5-c6f7-5f3d-ac8a-083b8ed9411b','captured-fixture',1,income,tax,shipping,cash);
 IF prepared->>'status'<>'refund_pending' OR (prepared->>'refund_minor')::bigint<>120
  OR (prepared->>'income_minor')::bigint<>100 OR (prepared->>'tax_minor')::bigint<>10
  OR (prepared->>'shipping_minor')::bigint<>10 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE server refund allocation';
 END IF;
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'444723f5-c6f7-5f3d-ac8a-083b8ed9411b','captured-fixture',uuidv7());
  RAISE EXCEPTION 'FULFILLMENT_GATE unposted/foreign ledger accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
 journal:=app.post_ledger(t,'USD','refund-probe',cancel_id::text,'wrong-receipt',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[1,1]::bigint[]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 BEGIN
  PERFORM app.returns_complete_refund(cancel_id,'444723f5-c6f7-5f3d-ac8a-083b8ed9411b','captured-fixture',journal);
  RAISE EXCEPTION 'FULFILLMENT_GATE posted wrong journal accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
 journal:=app.post_ledger(t,'USD','return',cancel_id::text,'refund',ARRAY[income,tax,shipping,cash],
  ARRAY['debit','debit','debit','credit'],ARRAY[100,10,10,120]::bigint[]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 completed:=app.returns_complete_refund(cancel_id,'444723f5-c6f7-5f3d-ac8a-083b8ed9411b','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF completed->>'status'<>'refunded' OR (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>120
  OR (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_refunded' THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund not atomic with order state';
 END IF;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 PERFORM app.returns_complete_refund(cancel_id,'444723f5-c6f7-5f3d-ac8a-083b8ed9411b','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>120 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund replay duplicated amount';
 END IF;
 EXECUTE 'SET LOCAL ROLE app_runtime';
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 shipped:=app.fulfillment_create_shipment(oid,ord.version,'b8982a19-9647-54c0-9002-7c718dc45a03','manual','TRACK-2',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
 IF (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'partially_refunded'
  OR (SELECT sum(quantity) FROM app.shipment_lines WHERE tenant_id=t AND order_line_id=lid)<>3 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE refund state blocked remaining shipment';
 END IF;
 SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 prepared:=app.returns_prepare_refund(return_id,ord.version,'6837caef-b6b1-5598-8155-724aa49847ad','captured-fixture',1,income,tax,shipping,cash);
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
 journal:=app.post_ledger(t,'USD','return',return_id::text,'refund',ARRAY[income,tax,shipping,cash],
  ARRAY['debit','debit','debit','credit'],ARRAY[100,10,10,120]::bigint[]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 PERFORM app.returns_complete_refund(return_id,'6837caef-b6b1-5598-8155-724aa49847ad','captured-fixture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 IF (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>240 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cumulative refund amount';
 END IF;
 IF app.returns_create(oid,create_version,'e66bbcd5-f69e-5c04-91a9-1f33e1af7582','shipment_return','received damaged',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1))) IS DISTINCT FROM returned THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE creation response drifted after refund'; END IF;
 IF app.returns_receive(return_id,receive_version,'35b5f949-2b9d-5cb2-9fc8-95a04269d498',false) IS DISTINCT FROM received THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE receive response drifted after refund'; END IF;
 BEGIN
  UPDATE app.shipments SET tracking_number='3122fa34-c798-53de-8f57-4223245e2ac0' WHERE tenant_id=t;
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
  PERFORM app.returns_receive(return_id,1,'18c8f68d-408f-513e-a1f0-74fa09bbac61',true);
  RAISE EXCEPTION 'FULFILLMENT_GATE cross-tenant command';
 EXCEPTION WHEN no_data_found THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN
  PERFORM app.fulfillment_create_shipment(oid,1,'a8068149-78a6-54c4-90b4-8ae7b87530ab','manual','X','[]');
  RAISE EXCEPTION 'FULFILLMENT_GATE missing tenant context';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $fulfillment$;

-- Adversarial allocation: tiny components, unequal prices with a discount, and
-- a zero-price line. Each independently received unit has a monotone entitlement;
-- the final unit conserves every original financial component without drift.
RESET ROLE;
SELECT set_config('app.tenant_id',:'t',true);
-- Each active RMA state gets its own order: another RMA cannot accidentally
-- supply the fence. Old financial event receipts still replay, and the legacy
-- event path remains available before an RMA exists.
SET LOCAL ROLE app_runtime;
DO $active_rma$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid;sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid;lid uuid;rid uuid;sid uuid;res app.inventory_reservations;ord app.orders;
 shipment jsonb;rma jsonb;gate_i int;gate_name text;expected_status text;before_stock bigint;
BEGIN
 FOR gate_i IN 1..3 LOOP
  gate_name:=CASE gate_i WHEN 1 THEN 'cancel-received' WHEN 2 THEN 'physical-requested' ELSE 'physical-received' END;
  oid:=uuidv7();lid:=uuidv7();
  res:=app.inventory_reserve(sku,2,'order',oid,(md5(('active-rma-reserve-'||gate_i)::text)::uuid::text),900);
  ord:=app.orders_create(oid,(md5(('active-rma-order-'||gate_i)::text)::uuid::text),sha256(('active-rma'||gate_i)::bytea),NULL,
   '{"email":"active-rma@example.invalid"}','USD',0,'{"amount_minor":0,"currency":"USD","service":"fixture"}',
   jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
    'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',0,'line_total_minor',200,
    'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
  ord:=app.orders_transition(oid,'paid','active-rma-paid-'||gate_i,0);
  -- Positive witness: the pre-existing trusted financial event is preserved.
  ord:=app.orders_transition(oid,'partially_refunded','active-rma-prior-'||gate_i,10);
  IF ord.refunded_minor<>10 OR ord.state<>'partially_refunded' THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE no-RMA legacy financial event rejected'; END IF;
  IF gate_i>1 THEN
   shipment:=app.fulfillment_create_shipment(oid,ord.version,(md5(('active-rma-shipment-'||gate_i)::text)::uuid::text),'manual','RMA',
    jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
   sid:=(shipment->'lines'->0->>'id')::uuid;
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  END IF;
  SELECT on_hand INTO before_stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
  IF gate_i=1 THEN
   rma:=app.returns_create(oid,ord.version,(md5(('active-rma-create-'||gate_i)::text)::uuid::text),'unshipped_cancel','cancel paid remainder',
    jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
  ELSE
   rma:=app.returns_create(oid,ord.version,(md5(('active-rma-create-'||gate_i)::text)::uuid::text),'shipment_return','return shipped unit',
    jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
  END IF;
  rid:=(rma->>'id')::uuid;
  SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  IF gate_i=3 THEN
   rma:=app.returns_receive(rid,ord.version,(md5(('active-rma-receive-'||gate_i)::text)::uuid::text),true);
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  END IF;
  expected_status:=CASE gate_i WHEN 2 THEN 'requested' ELSE 'received' END;
  IF rma->>'status'<>expected_status OR (gate_i<>2 AND
   ((rma->>'restocked')::boolean IS DISTINCT FROM true OR
    (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>before_stock+1)) THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE active RMA % fixture failed',gate_name; END IF;
  -- The old receipt predates the RMA; replay must not be rejected or add money.
  IF app.orders_transition(oid,'partially_refunded','active-rma-prior-'||gate_i,10) IS DISTINCT FROM ord THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE active RMA % prior event replay drifted',gate_name; END IF;
  BEGIN
   PERFORM app.orders_transition(oid,'refunded','active-rma-new-'||gate_i,ord.total_minor-ord.refunded_minor);
   RAISE EXCEPTION 'FULFILLMENT_GATE active RMA % accepted direct refund',gate_name;
  EXCEPTION WHEN object_not_in_prerequisite_state THEN
   IF SQLERRM<>'FULFILLMENT_PENDING_REFUND_COMMAND_REQUIRED' THEN RAISE; END IF;
  END;
  IF (SELECT row(o.*) FROM app.orders o WHERE tenant_id=t AND id=oid) IS DISTINCT FROM row(ord.*)
   OR (SELECT status FROM app.returns WHERE tenant_id=t AND id=rid)<>expected_status
   OR EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND delivery_target='orders.events'
    AND dedup_key='transition:'||oid::text||':active-rma-new-'||gate_i) THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE active RMA % denied event changed state',gate_name; END IF;
  RAISE NOTICE 'PASS: active RMA % financial event fence, prior replay and no-RMA legacy path',gate_name;
 END LOOP;
END $active_rma$;
-- Legacy refunds consume the same financial entitlement as RMA refunds. A full
-- cancellation must freeze the remaining 190, including when split into two RMAs.
DO $legacy_conservation$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid;sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid;lid uuid;res app.inventory_reservations;ord app.orders;r jsonb;
 income uuid:=uuidv7();tax uuid:=uuidv7();shipping uuid:=uuidv7();cash uuid:=uuidv7();journal uuid;
 case_i int;part_i int;parts int;amount bigint;initial_stock bigint;rid uuid;
BEGIN
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'legacy-income','USD','revenue'),(t,tax,'legacy-tax','USD','liability'),
  (t,shipping,'legacy-shipping','USD','revenue'),(t,cash,'legacy-cash','USD','asset');
 FOR case_i IN 1..2 LOOP
  oid:=uuidv7();lid:=uuidv7();parts:=case_i;
  SELECT on_hand INTO initial_stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
  res:=app.inventory_reserve(sku,2,'order',oid,(md5(('legacy-reserve-'||case_i)::text)::uuid::text),900);
  ord:=app.orders_create(oid,(md5(('legacy-order-'||case_i)::text)::uuid::text),sha256(('83d8c44f-a133-5a43-b196-1565e9318c2d'||case_i)::bytea),NULL,
   '{"email":"legacy@example.invalid"}','USD',0,'{"amount_minor":0,"currency":"USD","service":"fixture"}',
   jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
    'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',0,'line_total_minor',200,
    'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
  ord:=app.orders_transition(oid,'paid','legacy-paid-'||case_i,0);
  ord:=app.orders_transition(oid,'partially_refunded','legacy-credit-'||case_i,10);
  FOR part_i IN 1..parts LOOP
   r:=app.returns_create(oid,ord.version,(md5(('legacy-rma-'||case_i||'-'||part_i)::text)::uuid::text),'unshipped_cancel','remaining credit',
    jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2/parts)));
   rid:=(r->>'id')::uuid;amount:=190/parts;
   IF (r->>'refund_minor')::bigint<>amount THEN
    RAISE EXCEPTION 'FULFILLMENT_GATE legacy credit frozen amount: got %, expected %',r->>'refund_minor',amount;END IF;
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
   EXECUTE 'SET LOCAL ROLE app_returns_service';
   r:=app.returns_prepare_refund(rid,ord.version,(md5(('legacy-refund-'||case_i||'-'||part_i)::text)::uuid::text),'legacy-capture',1,income,tax,shipping,cash);
   IF (r->>'refund_minor')::bigint<>amount THEN RAISE EXCEPTION 'FULFILLMENT_GATE legacy freeze drift';END IF;
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
   journal:=app.post_ledger(t,'USD','return',rid::text,'refund',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[amount,amount]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
   PERFORM app.returns_complete_refund(rid,(md5(('legacy-refund-'||case_i||'-'||part_i)::text)::uuid::text),'legacy-capture',journal);
   EXECUTE 'SET LOCAL ROLE app_runtime';
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  END LOOP;
  IF ord.refunded_minor<>200 OR ord.state<>'refunded'
   OR (SELECT sum(refund_minor) FROM app.returns WHERE tenant_id=t AND order_id=oid)<>190
   OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>initial_stock
   OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t AND owner_type='return'
    AND owner_id IN(SELECT id FROM app.returns WHERE tenant_id=t AND order_id=oid))<>parts THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE legacy/RMA conservation or restore-once failed case %',case_i;END IF;
 END LOOP;
 RAISE NOTICE 'PASS: legacy 10 + RMA 190 and legacy 10 + RMA 95 + RMA 95';
END $legacy_conservation$;
-- Cross-line frozen capacity cannot be reintroduced by a later legacy event.
-- Hamilton([5,3,1],5)=[3,2,0], but Hamilton([5,3,1],4)=[2,1,1]:
-- recomputing original caps would erase/reintroduce the fully returned last line.
-- Freeze its 1 first, then legacy credits 4 and 1 consume only [5,3,0], yielding
-- stable remaining caps [0,3,0] in UUID order. All three lines use one stock SKU.
DO $cross_line_legacy$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid;sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid:=uuidv7();lids uuid[]:=ARRAY['00000000-0000-4000-8000-000000000101',
  '00000000-0000-4000-8000-000000000102','00000000-0000-4000-8000-000000000103']::uuid[];
 caps bigint[]:=ARRAY[5,3,1]::bigint[];res app.inventory_reservations;ord app.orders;lines jsonb:='[]';
 income uuid:=uuidv7();tax uuid:=uuidv7();shipping uuid:=uuidv7();cash uuid:=uuidv7();journal uuid;
 r jsonb;first_rid uuid;rid uuid;i int;line_i int;expected bigint;initial_stock bigint;
BEGIN
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'cross-line-income','USD','revenue'),(t,tax,'cross-line-tax','USD','liability'),
  (t,shipping,'cross-line-shipping','USD','revenue'),(t,cash,'cross-line-cash','USD','asset');
 SELECT on_hand INTO initial_stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
 FOR i IN 1..3 LOOP
  res:=app.inventory_reserve(sku,1,'order',oid,(md5(('cross-line-reserve-'||i)::text)::uuid::text),900);
  lines:=lines||jsonb_build_array(jsonb_build_object('id',lids[i],'sku_id',sku,'sku','fulfillment-sku',
   'quantity',1,'unit_price_minor',caps[i],'line_net_minor',caps[i],'line_tax_minor',0,
   'line_total_minor',caps[i],'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb));
 END LOOP;
 ord:=app.orders_create(oid,'d0f42c7e-b84c-5e77-b3a0-819378444041',sha256('cross-line'::bytea),NULL,
  '{"email":"cross-line@example.invalid"}','USD',0,'{"amount_minor":0,"currency":"USD","service":"fixture"}',lines,'[]');
 ord:=app.orders_transition(oid,'paid','cross-line-paid',0);
 -- Return the last UUID first; its immutable financial receipt consumes its cap.
 FOR i IN 1..3 LOOP
  line_i:=CASE i WHEN 1 THEN 3 WHEN 2 THEN 1 ELSE 2 END;
  expected:=CASE i WHEN 1 THEN 1 WHEN 2 THEN 0 ELSE 3 END;
  r:=app.returns_create(oid,ord.version,(md5(('cross-line-rma-'||i)::text)::uuid::text),'unshipped_cancel','cross-line remaining capacity',
   jsonb_build_array(jsonb_build_object('order_line_id',lids[line_i],'quantity',1)));
  rid:=(r->>'id')::uuid;
  IF i=1 THEN first_rid:=rid;END IF;
  IF (r->>'refund_minor')::bigint<>expected OR (r->>'restocked')::boolean IS DISTINCT FROM true THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE cross-line frozen cap: line % got % expected %',line_i,r->>'refund_minor',expected;END IF;
  SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  IF expected>0 THEN
   EXECUTE 'SET LOCAL ROLE app_returns_service';
   PERFORM app.returns_prepare_refund(rid,ord.version,(md5(('cross-line-refund-'||i)::text)::uuid::text),'cross-line-capture',1,income,tax,shipping,cash);
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
   journal:=app.post_ledger(t,'USD','return',rid::text,'refund',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[expected,expected]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
   PERFORM app.returns_complete_refund(rid,(md5(('cross-line-refund-'||i)::text)::uuid::text),'cross-line-capture',journal);
   EXECUTE 'SET LOCAL ROLE app_runtime';
  ELSIF r->>'status'<>'completed' OR EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND id=rid
    AND (refund_key IS NOT NULL OR ledger_transaction_id IS NOT NULL)) THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE cross-line zero cancellation is not terminal without journal';
  END IF;
  SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  IF i=1 THEN
   ord:=app.orders_transition(oid,'partially_refunded','cross-line-legacy-four',4);
   ord:=app.orders_transition(oid,'partially_refunded','cross-line-legacy-one',1);
   IF ord.refunded_minor<>6 THEN RAISE EXCEPTION 'FULFILLMENT_GATE cross-line legacy sequence failed';END IF;
  END IF;
 END LOOP;
 IF ord.refunded_minor<>9 OR ord.total_minor<>9 OR ord.state<>'refunded'
  OR (SELECT refund_minor FROM app.returns WHERE tenant_id=t AND id=first_rid)<>1
  OR (SELECT status FROM app.returns WHERE tenant_id=t AND id=first_rid)<>'refunded'
  OR (SELECT sum(refund_minor) FROM app.returns WHERE tenant_id=t AND order_id=oid)<>4
  OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>initial_stock
  OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t AND owner_type='return'
   AND owner_id IN(SELECT id FROM app.returns WHERE tenant_id=t AND order_id=oid))<>3
  OR (SELECT coalesce(sum(e.amount),0) FROM app.ledger_entries e JOIN app.returns rr
   ON rr.tenant_id=e.tenant_id AND rr.ledger_transaction_id=e.transaction_id
   WHERE rr.tenant_id=t AND rr.order_id=oid AND e.side='credit')<>4
  OR EXISTS(SELECT e.transaction_id FROM app.ledger_entries e JOIN app.returns rr
   ON rr.tenant_id=e.tenant_id AND rr.ledger_transaction_id=e.transaction_id
   WHERE rr.tenant_id=t AND rr.order_id=oid GROUP BY e.transaction_id
   HAVING sum(CASE e.side WHEN 'debit' THEN e.amount ELSE -e.amount END)<>0) THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE cross-line legacy frozen credit conservation/restoration failed';END IF;
 RAISE NOTICE 'PASS: cross-line caps [5,3,1] freeze 1, legacy 4+1, remaining [0,3,0] conserve 9';
END $cross_line_legacy$;
-- A discounted two-unit order has entitlements [0,1]. Receive/refund the
-- positive unit first, then receive the zero unit after the order is refunded.
DO $zero_after_refund$
DECLARE
 t uuid:=current_setting('test.fulfillment_t')::uuid;sku uuid:=current_setting('test.fulfillment_sku')::uuid;
 oid uuid:=uuidv7();lid uuid:=uuidv7();res app.inventory_reservations;ord app.orders;
 shipment jsonb;a jsonb;b jsonb;received jsonb;sid uuid;aid uuid;bid uuid;receive_version bigint;
 income uuid:=uuidv7();tax uuid:=uuidv7();shipping uuid:=uuidv7();cash uuid:=uuidv7();journal uuid;initial_stock bigint;
BEGIN
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'zero-income','USD','revenue'),(t,tax,'zero-tax','USD','liability'),
  (t,shipping,'zero-shipping','USD','revenue'),(t,cash,'zero-cash','USD','asset');
 SELECT on_hand INTO initial_stock FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku;
 res:=app.inventory_reserve(sku,2,'order',oid,'e5677381-4745-5127-b546-d73aae3703e2',900);
 ord:=app.orders_create(oid,'6801961a-5e04-5d18-a977-e845f9d207a6',sha256('5997dec8-e818-58d2-93dc-99dd4d35dd0f'::bytea),NULL,
  '{"email":"zero@example.invalid"}','USD',0,'{"amount_minor":0,"currency":"USD","service":"fixture"}',
  jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
   'unit_price_minor',1,'line_net_minor',2,'line_tax_minor',0,'line_total_minor',2,
   'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb)),
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'kind','discount','amount_minor',-1,'reason','zero fixture')));
 ord:=app.orders_transition(oid,'paid','zero-paid',0);
 shipment:=app.fulfillment_create_shipment(oid,ord.version,'2839c908-6c21-55c7-950a-e67cd59ce710','manual','ZERO',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',2)));
 sid:=(shipment->'lines'->0->>'id')::uuid;
 SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
 a:=app.returns_create(oid,ord.version,'c9a583b5-8433-533b-8eb1-3d27fdb65759','shipment_return','zero unit',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
 aid:=(a->>'id')::uuid;
 SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
 b:=app.returns_create(oid,ord.version,'3a29790d-00ff-591a-8360-dc3af8ba2c93','shipment_return','positive unit',
  jsonb_build_array(jsonb_build_object('order_line_id',lid,'shipment_line_id',sid,'quantity',1)));
 bid:=(b->>'id')::uuid;
 IF (a->>'refund_minor')::bigint<>0 OR (b->>'refund_minor')::bigint<>1 THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE zero fixture entitlements not [0,1]';END IF;
 SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
 PERFORM app.returns_receive(bid,ord.version,'69f9d1e2-d6b6-59b6-baba-de948cfd3870',true);
 SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 PERFORM app.returns_prepare_refund(bid,ord.version,'a6b78b9d-039c-5b0d-a750-386357bd8bd5','zero-capture',1,income,tax,shipping,cash);
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
 journal:=app.post_ledger(t,'USD','return',bid::text,'refund',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[1,1]::bigint[]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
 PERFORM app.returns_complete_refund(bid,'a6b78b9d-039c-5b0d-a750-386357bd8bd5','zero-capture',journal);
 EXECUTE 'SET LOCAL ROLE app_runtime';
 SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
 IF ord.state<>'refunded' THEN RAISE EXCEPTION 'FULFILLMENT_GATE zero positive unit did not refund order';END IF;
 receive_version:=ord.version;
 received:=app.returns_receive(aid,receive_version,'7d3e9387-734f-5acf-8eca-cdb384c64b92',true);
 IF received->>'status'<>'completed' OR (received->>'restocked')::boolean IS DISTINCT FROM true THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE zero return lacks terminal completed receipt';END IF;
 IF app.returns_receive(aid,receive_version,'7d3e9387-734f-5acf-8eca-cdb384c64b92',true) IS DISTINCT FROM received THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE zero receive replay drift';END IF;
 IF (SELECT state FROM app.orders WHERE tenant_id=t AND id=oid)<>'refunded'
  OR (SELECT refunded_minor FROM app.orders WHERE tenant_id=t AND id=oid)<>1
  OR (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku)<>initial_stock
  OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t AND owner_type='return' AND owner_id=aid)<>1
  OR EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND id=aid AND (refund_key IS NOT NULL OR ledger_transaction_id IS NOT NULL))
  OR EXISTS(SELECT FROM app.ledger_transactions WHERE tenant_id=t AND source_type='return' AND source_id=aid::text) THEN
  RAISE EXCEPTION 'FULFILLMENT_GATE zero receive changed money/order or repeated restore';END IF;
 RAISE NOTICE 'PASS: zero RMA receive after refunded order terminal, restore-once, no financial journal';
END $zero_after_refund$;
RESET ROLE;
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
 case_i int;unit_i int;n bigint;tx bigint;sh bigint;expected bigint[];actual bigint[];raw_shipping bigint;
BEGIN
 EXECUTE 'SET LOCAL ROLE app_runtime';
 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income,'allocation-income','USD','revenue'),(t,tax,'allocation-tax','USD','liability'),
  (t,shipping,'allocation-shipping','USD','revenue'),(t,cash,'allocation-cash','USD','asset');
 FOR case_i IN 1..4 LOOP
  oid:=uuidv7();lid:=uuidv7();other_lid:=uuidv7();
  IF case_i=1 THEN
   expected:=ARRAY[5,3,1];
   res:=app.inventory_reserve(sku,9,'order',oid,'c33a8eef-a83b-522c-ba55-6e1ee6bb64c0',900);
   lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',9,
    'unit_price_minor',1,'line_net_minor',9,'line_tax_minor',3,'line_total_minor',12,'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb));
  ELSIF case_i=4 THEN
   -- UUID order matters: excess discount20 consumes first-line tax3 then shipping17.
   lid:='00000000-0000-4000-8000-000000000411';other_lid:='00000000-0000-4000-8000-000000000412';
   expected:=ARRAY[0,7,43];
   res:=app.inventory_reserve(sku,2,'order',oid,'622ec6e7-6451-5254-944b-991ba299d123',900);
   res2:=app.inventory_reserve(sku2,2,'order',oid,'54032304-942c-5a18-bada-7229fcf97ef4',900);
   lines:=jsonb_build_array(jsonb_build_object('id',other_lid,'sku_id',sku2,'sku','fulfillment-sku-2','quantity',2,
    'unit_price_minor',25,'line_net_minor',50,'line_tax_minor',7,'line_total_minor',57,'reservation_id',res2.id,'tax_snapshot','{"category":"standard"}'::jsonb),
    jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
    'unit_price_minor',25,'line_net_minor',50,'line_tax_minor',3,'line_total_minor',53,'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb));
  ELSE
   expected:=CASE case_i WHEN 2 THEN ARRAY[25,8,5] ELSE ARRAY[0,0,4] END;
   IF case_i=2 THEN
    res:=app.inventory_reserve(sku,2,'order',oid,'b295f721-3ca5-5acf-a991-f34dacd5a742',900);
    res2:=app.inventory_reserve(sku2,2,'order',oid,'f211f75f-fe6d-5b33-8f99-c2ea0ca9c5f0',900);
    lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',2,
     'unit_price_minor',5,'line_net_minor',10,'line_tax_minor',3,'line_total_minor',13,'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb),
     jsonb_build_object('id',other_lid,'sku_id',sku2,'sku','fulfillment-sku-2','quantity',2,
     'unit_price_minor',9,'line_net_minor',18,'line_tax_minor',5,'line_total_minor',23,'reservation_id',res2.id,'tax_snapshot','{"category":"standard"}'::jsonb));
   ELSE
    res:=app.inventory_reserve(sku,4,'order',oid,'91011e26-c39d-5d71-b8ca-db79da348538',900);
    lines:=jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','fulfillment-sku','quantity',4,
     'unit_price_minor',0,'line_net_minor',0,'line_tax_minor',0,'line_total_minor',0,'reservation_id',res.id,'tax_snapshot','{"category":"standard"}'::jsonb));
   END IF;
  END IF;
  raw_shipping:=CASE case_i WHEN 4 THEN 60 ELSE expected[3] END;
  ord:=app.orders_create(oid,(md5(('allocation-order-'||case_i)::text)::uuid::text),sha256(('allocation'||case_i)::bytea),NULL,
   '{"email":"allocation@example.invalid"}','USD',raw_shipping,jsonb_build_object('currency','USD','amount_minor',raw_shipping,'service','fixture'),
   lines,CASE WHEN case_i IN(1,2,4) THEN jsonb_build_array(jsonb_build_object('id',uuidv7(),'kind','discount','amount_minor',CASE case_i WHEN 1 THEN -4 WHEN 4 THEN -120 ELSE -3 END,'reason','fixture discount')) ELSE '[]'::jsonb END);
  ord:=app.orders_transition(oid,'paid','allocation-paid-'||case_i,0);actual:=ARRAY[0,0,0]::bigint[];
  FOR unit_i IN 1..CASE case_i WHEN 1 THEN 9 ELSE 4 END LOOP
   chosen_id:=CASE WHEN case_i IN(2,4) AND unit_i%2=0 THEN other_lid ELSE lid END;
   r:=app.returns_create(oid,ord.version,(md5(('allocation-return-'||case_i||'-'||unit_i)::text)::uuid::text),'unshipped_cancel','unit entitlement',
    jsonb_build_array(jsonb_build_object('order_line_id',chosen_id,'quantity',1)));
   n:=(r->>'income_minor')::bigint;tx:=(r->>'tax_minor')::bigint;sh:=(r->>'shipping_minor')::bigint;
   IF n<0 OR tx<0 OR sh<0 THEN RAISE EXCEPTION 'FULFILLMENT_GATE nonmonotone tiny entitlement';END IF;
   IF case_i=4 AND ARRAY[n,tx,sh]<>(CASE unit_i WHEN 1 THEN ARRAY[0,0,6] WHEN 2 THEN ARRAY[0,3,15]
     WHEN 3 THEN ARRAY[0,0,7] ELSE ARRAY[0,4,15] END)::bigint[] THEN
    RAISE EXCEPTION 'FULFILLMENT_GATE excess discount tuple order or cumulative rounding unit % actual %',unit_i,ARRAY[n,tx,sh];END IF;
   actual:=ARRAY[actual[1]+n,actual[2]+tx,actual[3]+sh];
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
   IF (r->>'refund_minor')::bigint>0 THEN
    EXECUTE 'SET LOCAL ROLE app_returns_service';
    r:=app.returns_prepare_refund((r->>'id')::uuid,ord.version,(md5(('allocation-refund-'||case_i||'-'||unit_i)::text)::uuid::text),'capture-allocation',1,income,tax,shipping,cash);
    accounts:=ARRAY[]::uuid[];sides:=ARRAY[]::text[];amounts:=ARRAY[]::bigint[];
    IF n>0 THEN accounts:=array_append(accounts,income);sides:=array_append(sides,'debit');amounts:=array_append(amounts,n);END IF;
    IF tx>0 THEN accounts:=array_append(accounts,tax);sides:=array_append(sides,'debit');amounts:=array_append(amounts,tx);END IF;
    IF sh>0 THEN accounts:=array_append(accounts,shipping);sides:=array_append(sides,'debit');amounts:=array_append(amounts,sh);END IF;
    accounts:=array_append(accounts,cash);sides:=array_append(sides,'credit');amounts:=array_append(amounts,n+tx+sh);
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
    journal:=app.post_ledger(t,'USD','return',r->>'id','refund',accounts,sides,amounts);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
    PERFORM app.returns_complete_refund((r->>'id')::uuid,(md5(('allocation-refund-'||case_i||'-'||unit_i)::text)::uuid::text),'capture-allocation',journal);
    EXECUTE 'SET LOCAL ROLE app_runtime';
   END IF;
   SELECT * INTO ord FROM app.orders WHERE tenant_id=t AND id=oid;
  END LOOP;
  IF actual<>expected OR ord.refunded_minor<>ord.total_minor OR ord.state<>'refunded' THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE financial conservation case % actual % expected %',case_i,actual,expected;END IF;
  IF case_i=4 AND ord.total_minor<>50 THEN
   RAISE EXCEPTION 'FULFILLMENT_GATE excess discount total % expected 50',ord.total_minor;END IF;
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

-- Refund coordinator may complete only a pre-existing, current C04 refundReturn claim.
BEGIN;
INSERT INTO app.tenants(name) VALUES('refund-C04-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('refund-C04-B') RETURNING id AS tb \gset
SELECT uuidv7() AS actor,uuidv7() AS claim \gset
SELECT set_config('test.refund_c04_t',:'ta',true),set_config('test.refund_c04_other',:'tb',true),
 set_config('test.refund_c04_actor',:'actor',true),set_config('test.refund_c04_claim',:'claim',true),
 set_config('app.tenant_id',:'ta',true);
SET LOCAL ROLE app_runtime;
SELECT app.idempotency_claim(:'ta','api_key',:'actor','refundReturn','96bff2a2-faf6-5416-8656-d84d4c64712f',sha256('96bff2a2-faf6-5416-8656-d84d4c64712f'::bytea),interval '1 hour',:'claim');
SELECT app.idempotency_claim(:'ta','api_key',:'actor','createShipment','a261d7b6-99af-55e3-8fe8-4ea29e72dd8b',sha256('other-op'::bytea),interval '1 hour',uuidv7());
SELECT set_config('app.tenant_id',:'tb',true);
SELECT app.idempotency_claim(:'tb','api_key',:'actor','refundReturn','3d557673-cc15-5fbc-9c67-66542f4e9ed4',sha256('3d557673-cc15-5fbc-9c67-66542f4e9ed4'::bytea),interval '1 hour',uuidv7());
SELECT set_config('app.tenant_id',:'ta',true);
SET LOCAL ROLE app_returns_service;
DO $refund_c04$
DECLARE t uuid:=current_setting('test.refund_c04_t')::uuid; other_t uuid:=current_setting('test.refund_c04_other')::uuid;
 actor uuid:=current_setting('test.refund_c04_actor')::uuid; token uuid:=current_setting('test.refund_c04_claim')::uuid; n bigint;
BEGIN
 BEGIN
  SELECT count(tenant_id) INTO n FROM app.idempotency_records;
  IF n<>1 THEN RAISE EXCEPTION 'FULFILLMENT_C04_GATE coordinator sees foreign operation/tenant'; END IF;
 EXCEPTION WHEN insufficient_privilege THEN RAISE EXCEPTION 'FULFILLMENT_C04_GATE refund completion unavailable'; END;
 UPDATE app.idempotency_records SET state='in_progress' WHERE tenant_id=t AND operation='createShipment';
 GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>0 THEN RAISE EXCEPTION 'FULFILLMENT_C04_GATE coordinator updated foreign operation'; END IF;
 UPDATE app.idempotency_records SET state='in_progress' WHERE tenant_id=other_t;
 GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>0 THEN RAISE EXCEPTION 'FULFILLMENT_C04_GATE coordinator updated foreign tenant'; END IF;
 BEGIN
  INSERT INTO app.idempotency_records(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at)
  VALUES(t,'api_key',actor,'refundReturn','3122fa34-c798-53de-8f57-4223245e2ac0',sha256('3122fa34-c798-53de-8f57-4223245e2ac0'::bytea),clock_timestamp()+interval '1 hour');
  RAISE EXCEPTION 'FULFILLMENT_C04_GATE coordinator claimed new intent';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.idempotency_records SET claim_token=uuidv7() WHERE tenant_id=t;
  RAISE EXCEPTION 'FULFILLMENT_C04_GATE coordinator changed claim token';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.idempotency_claim',uuidv7()::text,true);
 BEGIN
  UPDATE app.idempotency_records SET state='completed',response_status=200,
   response_body_reference='fixture:refund',completed_at=clock_timestamp()
  WHERE tenant_id=t AND actor_kind='api_key' AND actor_id=actor AND operation='refundReturn'
   AND idempotency_key='96bff2a2-faf6-5416-8656-d84d4c64712f' AND request_hash=sha256('96bff2a2-faf6-5416-8656-d84d4c64712f'::bytea)
   AND claim_token=token AND state='in_progress';
  RAISE EXCEPTION 'FULFILLMENT_C04_GATE stale claim completed';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 PERFORM set_config('app.idempotency_claim',token::text,true);
 UPDATE app.idempotency_records SET state='completed',response_status=200,
  response_body_reference='fixture:refund',completed_at=clock_timestamp()
 WHERE tenant_id=t AND actor_kind='api_key' AND actor_id=actor AND operation='refundReturn'
  AND idempotency_key='96bff2a2-faf6-5416-8656-d84d4c64712f' AND request_hash=sha256('96bff2a2-faf6-5416-8656-d84d4c64712f'::bytea)
  AND claim_token=token AND state='in_progress';
 GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>1 THEN RAISE EXCEPTION 'FULFILLMENT_C04_GATE refund completion unavailable'; END IF;
END $refund_c04$;
RESET ROLE;
-- Poison only the target mode of the common command, keeping its OID/ACL and
-- signature. Other modes execute the unchanged original body. In particular,
-- completion must pass lock mode and reach complete; freeze is tested both
-- before creation and after its sealed receipt exists in the same transaction.
CREATE FUNCTION pg_temp.refund_budget_witness(t uuid) RETURNS jsonb LANGUAGE sql AS $$
 SELECT jsonb_build_object(
  'orders',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.orders x WHERE tenant_id=t),
  'returns',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.returns x WHERE tenant_id=t),
  'lines',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.return_lines x WHERE tenant_id=t),
  'stock',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY sku_id),'[]') FROM app.stock_levels x WHERE tenant_id=t),
  'movements',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.inventory_movements x WHERE tenant_id=t),
  'journals',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.ledger_transactions x WHERE tenant_id=t),
  'entries',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY transaction_id,line_no),'[]') FROM app.ledger_entries x WHERE tenant_id=t),
  'outbox',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM app.outbox_events x WHERE tenant_id=t));
$$;
DO $budget_intervention$
DECLARE command oid:='app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint)'::regprocedure;
 original text:=pg_get_functiondef(command); args text:=pg_get_function_identity_arguments(command);
 original_body text; trap text; phase text; target text;
 t uuid; product uuid; sku uuid; oid uuid; lid uuid; rid uuid; journal uuid;
 income uuid; tax uuid; shipping uuid; cash uuid; reservation app.inventory_reservations; ord app.orders;
 r jsonb; before jsonb; hit boolean;
BEGIN
 SELECT regexp_replace(prosrc,'[[:space:]]+$','') INTO original_body FROM pg_proc WHERE pg_proc.oid=command;
 -- A nested original PL/pgSQL block retains all declarations and RETURNs.
 IF right(original_body,1)<>';' THEN original_body:=original_body||';'; END IF;
 FOREACH phase IN ARRAY ARRAY['83d8c44f-a133-5a43-b196-1565e9318c2d','freeze_before','freeze_after','reserve','lock','complete'] LOOP
  target:=CASE WHEN starts_with(phase,'freeze_') THEN 'freeze' ELSE phase END;
  INSERT INTO app.tenants(name,settlement_currency) VALUES('budget intervention '||phase,'USD') RETURNING id INTO t;
  INSERT INTO app.products(tenant_id,product_code) VALUES(t,'budget') RETURNING id INTO product;
  INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(t,product,'budget','default') RETURNING id INTO sku;
  PERFORM set_config('app.tenant_id',t::text,true);
  PERFORM app.inventory_adjust(sku,20,'budget fixture',uuidv7(),'inbound');
  income:=uuidv7(); tax:=uuidv7(); shipping:=uuidv7(); cash:=uuidv7();
  INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
   (t,income,'sales','USD','revenue'),(t,tax,'tax','USD','liability'),
   (t,shipping,'shipping','USD','revenue'),(t,cash,'cash','USD','asset');
  oid:=uuidv7(); lid:=uuidv7();
  EXECUTE 'SET LOCAL ROLE app_runtime';
  reservation:=app.inventory_reserve(sku,2,'order',oid,'9b34ae62-cf87-5706-bf16-34700262a8a0',900);
  ord:=app.orders_create(oid,'c274ec69-57c5-5687-8e88-bfcf3af6012f',sha256('budget'::bytea),NULL,
   '{"email":"budget@example.invalid"}','USD',0,'{"amount_minor":0,"currency":"USD","service":"fixture"}',
   jsonb_build_array(jsonb_build_object('id',lid,'sku_id',sku,'sku','budget','quantity',2,
    'unit_price_minor',10,'line_net_minor',20,'line_tax_minor',0,'line_total_minor',20,
    'reservation_id',reservation.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
  ord:=app.orders_transition(oid,'paid','budget-paid',0);
  IF phase IN('reserve','lock','complete') THEN
   r:=app.returns_create(oid,ord.version,'73b5f4ea-07cb-5402-a65f-e736c8cfe19e','unshipped_cancel','budget test',
    jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
   rid:=(r->>'id')::uuid;
   SELECT version INTO ord.version FROM app.orders WHERE tenant_id=t AND id=oid;
  END IF;
  IF phase IN('lock','complete') THEN
   EXECUTE 'SET LOCAL ROLE app_returns_service';
   r:=app.returns_prepare_refund(rid,ord.version,'143e9905-be83-5c2c-8254-97873cbf0fa9','budget-provider',1,income,tax,shipping,cash);
  END IF;
  IF phase='complete' THEN
 EXECUTE 'SET LOCAL ROLE app_runtime'; -- privileged fixture journal, never Returns posting authority
   journal:=app.post_ledger(t,'USD','return',rid::text,'refund',ARRAY[income,cash],ARRAY['debit','credit'],ARRAY[10,10]::bigint[]);
 EXECUTE 'SET LOCAL ROLE app_returns_service';
  END IF;
  EXECUTE 'RESET ROLE';
  before:=pg_temp.refund_budget_witness(t);
  IF phase='reserve' THEN
   -- This is a nonempty, tenant-visible RMA. Column lock authority must not
   -- permit even a no-op SQL UPDATE to bypass the header command guard.
   IF NOT EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND id=rid) THEN
    RAISE EXCEPTION 'SHARED_REFUND_BUDGET_GUARD_EMPTY_FIXTURE'; END IF;
   hit:=false;
   BEGIN
    EXECUTE 'SET LOCAL ROLE app_orders_executor';
    UPDATE app.returns SET id=id WHERE tenant_id=t AND id=rid;
   EXCEPTION WHEN SQLSTATE '55000' THEN
    IF SQLERRM<>'FULFILLMENT_CONTROLLED_COMMAND_REQUIRED' THEN RAISE; END IF;
    hit:=true;
   END;
   EXECUTE 'RESET ROLE';
   IF NOT hit THEN RAISE EXCEPTION 'SHARED_REFUND_BUDGET_ORDERS_MUTATION_ALLOWED'; END IF;
   IF pg_temp.refund_budget_witness(t) IS DISTINCT FROM before THEN
    RAISE EXCEPTION 'SHARED_REFUND_BUDGET_GUARD_ROLLBACK'; END IF;
   RAISE NOTICE 'PASS: Orders lock-only UPDATE(id) reaches nonempty RMA header guard 55000';
  END IF;
  trap:=format('$2=%L',target);
  IF phase='freeze_before' THEN
   trap:=trap||' AND NOT EXISTS(SELECT FROM app.returns WHERE tenant_id=app.orders_context() AND order_id=$1 AND sealed)';
  ELSIF phase='freeze_after' THEN
   trap:=trap||' AND EXISTS(SELECT FROM app.returns WHERE tenant_id=app.orders_context() AND order_id=$1 AND sealed)';
  END IF;
  EXECUTE format('CREATE OR REPLACE FUNCTION app.orders_refund_budget(%s) RETURNS app.orders
   LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $sentinel$
   BEGIN
    IF %s THEN RAISE EXCEPTION ''SHARED_REFUND_BUDGET_SENTINEL'' USING ERRCODE=''P39B1''; END IF;
    %s
   END $sentinel$',args,trap,original_body);
  hit:=false;
  BEGIN
   IF target IN('83d8c44f-a133-5a43-b196-1565e9318c2d','freeze') THEN EXECUTE 'SET LOCAL ROLE app_runtime';
   ELSE EXECUTE 'SET LOCAL ROLE app_returns_service'; END IF;
   IF phase='legacy' THEN
    PERFORM app.orders_transition(oid,'partially_refunded','budget-legacy',2);
   ELSIF target='freeze' THEN
    PERFORM app.returns_create(oid,ord.version,'73b5f4ea-07cb-5402-a65f-e736c8cfe19e','unshipped_cancel','budget test',
     jsonb_build_array(jsonb_build_object('order_line_id',lid,'quantity',1)));
   ELSIF phase='reserve' THEN
    PERFORM app.returns_prepare_refund(rid,ord.version,'143e9905-be83-5c2c-8254-97873cbf0fa9','budget-provider',1,income,tax,shipping,cash);
   ELSIF phase='lock' THEN
    PERFORM app.returns_lock_refund(rid,'143e9905-be83-5c2c-8254-97873cbf0fa9');
   ELSE
    PERFORM app.returns_complete_refund(rid,'143e9905-be83-5c2c-8254-97873cbf0fa9','budget-provider',journal);
   END IF;
  EXCEPTION WHEN SQLSTATE 'P39B1' THEN hit:=true;
  END;
  EXECUTE 'RESET ROLE';
  IF NOT hit THEN RAISE EXCEPTION 'SHARED_REFUND_BUDGET_BYPASSED: %',phase; END IF;
  IF pg_temp.refund_budget_witness(t) IS DISTINCT FROM before THEN
   RAISE EXCEPTION 'SHARED_REFUND_BUDGET_ROLLBACK: %',phase;
  END IF;
  EXECUTE original;
  IF to_regprocedure('app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint)')::oid<>command THEN
   RAISE EXCEPTION 'SHARED_REFUND_BUDGET_PROBE_CHANGED_OID';
  END IF;
  RAISE NOTICE 'PASS: shared budget % public route reaches same OID and rolls back exact witnesses',phase;
 END LOOP;
END $budget_intervention$;
ROLLBACK;
SELECT 'PASS: refund coordinator completes only existing tenant refundReturn current claims' AS result;
