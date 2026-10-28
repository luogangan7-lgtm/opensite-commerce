-- Responsibility: real-PG order invariants; all fixtures roll back.
-- Depends on catalog, inventory and orders commands; run by check_pg.sh.
-- Invariants: immutable complete snapshots, tenant/owner fences, atomic transitions
-- and event-key replay cannot duplicate inventory effects or refund value.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('orders-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('orders-B') RETURNING id AS other_t \gset
-- Two local customers keep the immutability negative test valid under the FK;
-- the foreign customer is a real row, so rejection proves the composite tenant key.
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','orders-customer@example.invalid') RETURNING id AS customer \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','orders-alternate@example.invalid') RETURNING id AS alternate_customer \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'other_t','orders-foreign@example.invalid') RETURNING id AS foreign_customer \gset
SELECT set_config('test.orders_customer',:'customer',true),
 set_config('test.orders_alternate_customer',:'alternate_customer',true),
 set_config('test.orders_foreign_customer',:'foreign_customer',true);
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','orders') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'product','orders-sku','default') RETURNING id AS sku \gset
SELECT set_config('test.orders_t',:'t',true),set_config('test.orders_other_t',:'other_t',true),
 set_config('test.orders_sku',:'sku',true),set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',30,'orders receipt',uuidv7(),'inbound');
DO $constraint$ BEGIN
 -- Go store maps 23505 on this exact name to ErrRetry (RR create race).
 IF NOT EXISTS(SELECT FROM pg_constraint WHERE conrelid='app.orders'::regclass
  AND conname='orders_creation_key_unique' AND contype='u') THEN
  RAISE EXCEPTION 'ORDERS_GATE creation-key constraint name changed';
 END IF;
END $constraint$;
SET LOCAL ROLE app_runtime;
DO $orders$
DECLARE
 t uuid:=current_setting('test.orders_t')::uuid; sku uuid:=current_setting('test.orders_sku')::uuid;
 order_id_v uuid:=uuidv7(); other_id uuid:=uuidv7(); r app.inventory_reservations; o app.orders; replay app.orders;
 lines jsonb; guest jsonb:='{"email":"buyer@example.invalid"}'; shipping jsonb:='{"service":"fixture","amount_minor":20,"currency":"USD"}';
 starting_stock bigint; foreign_reservation uuid;
BEGIN
 r:=app.inventory_reserve(sku,2,'order',order_id_v,'c60c9981-3286-50e2-89e2-6357c9f0cab6',900);
 lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','frozen-sku','quantity',2,
  'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',10,'line_total_minor',210,
  'reservation_id',r.id,'tax_snapshot',jsonb_build_object('category','standard','net_minor',200,'tax_minor',10,'gross_minor',210,'currency','USD')));
 o:=app.orders_create(order_id_v,'f75d9c57-25bc-58d3-ba4a-83bde9572535',sha256('fixture'::bytea),NULL,guest,'USD',20,shipping,lines,'[]');
 IF o.state<>'pending_payment' OR NOT o.sealed OR o.total_minor<>230 OR o.order_number<>1 THEN
  RAISE EXCEPTION 'ORDERS_GATE creation snapshot or initial state';
 END IF;
 replay:=app.orders_create(order_id_v,'f75d9c57-25bc-58d3-ba4a-83bde9572535',sha256('fixture'::bytea),NULL,guest,'USD',20,shipping,lines,'[]');
 IF replay.id<>o.id OR (SELECT count(*) FROM app.orders)<>1 THEN
  RAISE EXCEPTION 'ORDERS_GATE creation replay duplicated order';
 END IF;
 BEGIN
  PERFORM app.orders_create(order_id_v,'f75d9c57-25bc-58d3-ba4a-83bde9572535',sha256('changed'::bytea),NULL,guest,'USD',20,shipping,lines,'[]');
  RAISE EXCEPTION 'ORDERS_GATE changed creation hash accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  UPDATE app.orders SET total_minor=1 WHERE app.orders.id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE runtime amount UPDATE';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.order_lines SET unit_price_minor=1;
  RAISE EXCEPTION 'ORDERS_GATE runtime line UPDATE';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.paid','{}','orders.events','forged-event');
  RAISE EXCEPTION 'ORDERS_GATE forged order outbox accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.inventory_commit(r.id);
  RAISE EXCEPTION 'ORDERS_GATE runtime bypasses order commit ownership';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.inventory_release(r.id);
  RAISE EXCEPTION 'ORDERS_GATE runtime bypasses order release ownership';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'fulfilled','early-fulfill',0);
  RAISE EXCEPTION 'ORDERS_GATE illegal transition accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Wrong owner fails before any order/line/event is persisted.
 r:=app.inventory_reserve_non_order(sku,1,'checkout',uuidv7(),'604c59a9-01e1-55a4-a4eb-e9444e3a8422',900,
  sha256(convert_to(uuidv4()::text||uuidv4()::text||uuidv4()::text,'UTF8')));
 foreign_reservation:=r.id;
 BEGIN
  PERFORM app.orders_create(other_id,'df1f31a5-be53-5749-8eca-a1e1185fc465',sha256('3122fa34-c798-53de-8f57-4223245e2ac0'::bytea),NULL,guest,'USD',20,shipping,
   jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','3122fa34-c798-53de-8f57-4223245e2ac0','quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
  RAISE EXCEPTION 'ORDERS_GATE forged reservation owner accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 IF EXISTS(SELECT FROM app.orders WHERE app.orders.id=other_id)
  OR EXISTS(SELECT FROM app.outbox_events WHERE payload->>'order_id'=other_id::text) THEN
  RAISE EXCEPTION 'ORDERS_GATE rejected order left partial snapshot';
 END IF;
 -- Prelock fence: runtime may lock only reservations owned by this order.
 PERFORM app.orders_lock_reservations(other_id,ARRAY[]::uuid[]);
 BEGIN
  PERFORM app.orders_lock_reservations(other_id,ARRAY[foreign_reservation]);
  RAISE EXCEPTION 'ORDERS_GATE prelock accepted foreign reservation';
 EXCEPTION WHEN insufficient_privilege THEN
  IF SQLERRM<>'ORDER_RESERVATION_OWNER_MISMATCH' THEN RAISE; END IF;
 END;
 BEGIN
  PERFORM app.orders_lock_reservations(other_id,ARRAY[uuidv7()]);
  RAISE EXCEPTION 'ORDERS_GATE prelock accepted missing reservation';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',current_setting('test.orders_other_t'),true);
 IF EXISTS(SELECT FROM app.orders WHERE app.orders.id=order_id_v) THEN
  RAISE EXCEPTION 'ORDERS_GATE cross-tenant order visible';
 END IF;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'paid','foreign-pay',0);
  RAISE EXCEPTION 'ORDERS_GATE cross-tenant command accepted';
 EXCEPTION WHEN no_data_found THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM app.orders) THEN RAISE EXCEPTION 'ORDERS_GATE missing tenant visible'; END IF;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'paid','missing-pay',0);
  RAISE EXCEPTION 'ORDERS_GATE missing tenant command accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','not-a-uuid',true);
 BEGIN
  PERFORM app.orders_transition(order_id_v,'paid','malformed-pay',0);
  RAISE EXCEPTION 'ORDERS_GATE malformed tenant command accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',t::text,true);
 SELECT on_hand INTO starting_stock FROM app.stock_levels WHERE sku_id=sku;
 o:=app.orders_transition(order_id_v,'paid','pay-event',0);
 replay:=app.orders_transition(order_id_v,'paid','pay-event',0);
 IF replay.id<>o.id OR o.version<>2 OR (SELECT on_hand FROM app.stock_levels WHERE sku_id=sku)<>starting_stock-2 THEN
  RAISE EXCEPTION 'ORDERS_GATE paid replay committed twice';
 END IF;
 IF (SELECT state FROM app.inventory_reservations WHERE app.inventory_reservations.id=(lines->0->>'reservation_id')::uuid)<>'committed' THEN
  RAISE EXCEPTION 'ORDERS_GATE paid did not commit reservation';
 END IF;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'cancelled','pay-event',0);
  RAISE EXCEPTION 'ORDERS_GATE reused event target accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'cancelled','late-cancel',0);
  RAISE EXCEPTION 'ORDERS_GATE paid cancellation accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 o:=app.orders_transition(order_id_v,'partially_refunded','refund-1',100);
 replay:=app.orders_transition(order_id_v,'partially_refunded','refund-1',100);
 IF replay.refunded_minor<>100 THEN RAISE EXCEPTION 'ORDERS_GATE refund replay repeated amount'; END IF;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'partially_refunded','refund-1',101);
  RAISE EXCEPTION 'ORDERS_GATE reused event refund amount accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'refunded','too-much',131);
  RAISE EXCEPTION 'ORDERS_GATE excessive refund accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 o:=app.orders_transition(order_id_v,'refunded','refund-2',130);
 IF o.refunded_minor<>o.total_minor THEN RAISE EXCEPTION 'ORDERS_GATE full refund total'; END IF;
 IF (SELECT count(*) FROM app.outbox_events WHERE payload->>'order_id'=order_id_v::text)<>4 THEN
  RAISE EXCEPTION 'ORDERS_GATE outbox transition count/replay';
 END IF;
 PERFORM set_config('test.orders_id',order_id_v::text,true);
 PERFORM set_config('test.orders_owner_forgery',foreign_reservation::text,true);
END $orders$;
RESET ROLE;
-- Superuser fixtures prove DB triggers enforce snapshots/state independently of GRANTs.
DO $guards$
DECLARE order_id_v uuid:=current_setting('test.orders_id')::uuid; line app.order_lines;
BEGIN
 SELECT * INTO line FROM app.order_lines WHERE order_id=order_id_v;
 BEGIN
  UPDATE app.orders SET items_net_minor=items_net_minor+1,total_minor=total_minor+1,refunded_minor=refunded_minor+1 WHERE app.orders.id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE immutable parent trigger absent';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.orders SET shipping_snapshot='{"amount_minor":0}' WHERE app.orders.id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE immutable shipping snapshot absent';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.order_lines SET sku='changed' WHERE order_id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE immutable SKU snapshot absent';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.order_adjustments SET amount_minor=0 WHERE order_id=order_id_v;
  INSERT INTO app.order_adjustments(tenant_id,order_id,kind,amount_minor,reason)
   VALUES(line.tenant_id,order_id_v,'discount',-1,'late');
  RAISE EXCEPTION 'ORDERS_GATE sealed adjustment INSERT accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  INSERT INTO app.order_lines(tenant_id,order_id,sku_id,sku,quantity,unit_price_minor,line_net_minor,line_tax_minor,line_total_minor,reservation_id,tax_snapshot)
   VALUES(line.tenant_id,order_id_v,line.sku_id,line.sku,1,100,100,0,100,uuidv7(),'{}');
  RAISE EXCEPTION 'ORDERS_GATE sealed line INSERT accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  UPDATE app.orders SET state='pending_payment',refunded_minor=0,version=version+1 WHERE app.orders.id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE direct illegal state UPDATE accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  DELETE FROM app.orders WHERE app.orders.id=order_id_v;
  RAISE EXCEPTION 'ORDERS_GATE order DELETE accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $guards$;
-- More pending fixtures prove cancellation and terminal checks independently.
SET LOCAL ROLE app_runtime;
DO $pending$
DECLARE sku uuid:=current_setting('test.orders_sku')::uuid; order_id_v uuid; r app.inventory_reservations;
 o app.orders; lines jsonb; n integer;
BEGIN
 FOR n IN 1..3 LOOP
  order_id_v:=uuidv7();
  r:=app.inventory_reserve(sku,1,'order',order_id_v,(md5(('orders-pending-reserve-'||n)::text)::uuid::text),900);
  lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','pending-sku','quantity',1,
   'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb));
  o:=app.orders_create(order_id_v,(md5(('orders-pending-create-'||n)::text)::uuid::text),sha256(convert_to(n::text,'UTF8')),NULL,
   '{"email":"pending@example.invalid"}','USD',0,'{}',lines,'[]');
  IF o.order_number<>n+1 THEN RAISE EXCEPTION 'ORDERS_GATE nonmonotonic order number'; END IF;
  PERFORM set_config('test.orders_pending_'||n,order_id_v::text,true);
  PERFORM set_config('test.orders_res_'||n,r.id::text,true);
 END LOOP;
 o:=app.orders_transition(current_setting('test.orders_pending_1')::uuid,'cancelled','cancel-event',0);
 PERFORM app.orders_transition(o.id,'cancelled','cancel-event',0);
 IF o.state<>'cancelled' OR o.version<>2 OR
  (SELECT state FROM app.inventory_reservations WHERE app.inventory_reservations.id=current_setting('test.orders_res_1')::uuid)<>'released' THEN
  RAISE EXCEPTION 'ORDERS_GATE pending cancel did not release exactly once';
 END IF;
 BEGIN
  PERFORM app.orders_transition(o.id,'paid','pay-after-cancel',0);
  RAISE EXCEPTION 'ORDERS_GATE cancelled order paid';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $pending$;
RESET ROLE;
DO $direct_valid$
BEGIN
 BEGIN
  UPDATE app.orders SET state='paid',version=version+1
   WHERE id=current_setting('test.orders_pending_2')::uuid;
  RAISE EXCEPTION 'ORDERS_GATE direct legal state omitted inventory/outbox';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $direct_valid$;
-- Deliberate fixture corruption cannot evade the transition's repeated owner fence.
UPDATE app.inventory_reservations SET owner_id=uuidv7()
 WHERE id=current_setting('test.orders_res_2')::uuid;
SET LOCAL ROLE app_runtime;
DO $owner_transition$
DECLARE order_id_v uuid:=current_setting('test.orders_pending_2')::uuid; before_stock bigint; before_events bigint;
BEGIN
 SELECT on_hand INTO before_stock FROM app.stock_levels WHERE sku_id=current_setting('test.orders_sku')::uuid;
 SELECT count(*) INTO before_events FROM app.outbox_events;
 BEGIN
  PERFORM app.orders_transition(order_id_v,'paid','forged-owner-pay',0);
  RAISE EXCEPTION 'ORDERS_GATE transition owner forgery accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 IF (SELECT state FROM app.orders WHERE app.orders.id=order_id_v)<>'pending_payment'
  OR (SELECT on_hand FROM app.stock_levels WHERE sku_id=current_setting('test.orders_sku')::uuid)<>before_stock
  OR (SELECT count(*) FROM app.outbox_events)<>before_events THEN
  RAISE EXCEPTION 'ORDERS_GATE failed transition left side effects';
 END IF;
END $owner_transition$;
RESET ROLE;
UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second'
 WHERE id=current_setting('test.orders_res_3')::uuid;
SET LOCAL ROLE app_runtime;
DO $expiry_cancel$
DECLARE order_id_v uuid:=current_setting('test.orders_pending_3')::uuid; o app.orders;
BEGIN
 BEGIN
  PERFORM app.orders_transition(order_id_v,'paid','expired-pay',0);
  RAISE EXCEPTION 'ORDERS_GATE expired order reservation paid';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 SET LOCAL ROLE app_queue_worker;
 PERFORM app.inventory_expire_due(100);
 SET LOCAL ROLE app_runtime;
 o:=app.orders_transition(order_id_v,'cancelled','expired-cancel',0);
 IF o.state<>'cancelled' OR (SELECT state FROM app.inventory_reservations
  WHERE app.inventory_reservations.id=current_setting('test.orders_res_3')::uuid)<>'expired' THEN
  RAISE EXCEPTION 'ORDERS_GATE expired cancellation invalid';
 END IF;
END $expiry_cancel$;
-- Customer-backed orders share the sealed guest snapshot contract, with a real
-- composite FK. These run last so earlier monotonic-number assertions stay fixed.
DO $customer_orders$
DECLARE sku uuid:=current_setting('test.orders_sku')::uuid; order_id_v uuid:=uuidv7();
 foreign_order_id uuid:=uuidv7(); r app.inventory_reservations; o app.orders; replay app.orders;
 lines jsonb;
BEGIN
 r:=app.inventory_reserve(sku,1,'order',order_id_v,'84073703-c6f3-506f-80f5-4c05d81dc421',900);
 lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','customer-sku','quantity',1,
  'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
  'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb));
 o:=app.orders_create(order_id_v,'11ca928d-b31f-5f47-8176-8249eb26fff6',sha256('customer'::bytea),
  current_setting('test.orders_customer')::uuid,NULL,'USD',0,'{}',lines,'[]');
 replay:=app.orders_create(order_id_v,'11ca928d-b31f-5f47-8176-8249eb26fff6',sha256('customer'::bytea),
  current_setting('test.orders_customer')::uuid,NULL,'USD',0,'{}',lines,'[]');
 IF o.customer_id<>current_setting('test.orders_customer')::uuid OR o.guest_contact IS NOT NULL
  OR NOT o.sealed OR o.total_minor<>100 OR o.state<>'pending_payment' OR replay.id<>o.id
  OR (SELECT count(*) FROM app.orders WHERE app.orders.id=o.id)<>1 THEN
  RAISE EXCEPTION 'ORDERS_GATE valid customer snapshot/replay';
 END IF;
 PERFORM set_config('test.orders_customer_order',o.id::text,true);
 r:=app.inventory_reserve(sku,1,'order',foreign_order_id,'b24d7ada-2d12-5a77-a095-e57ee3093cd4',900);
 lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','foreign-customer-sku','quantity',1,
  'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
  'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb));
 BEGIN
  PERFORM app.orders_create(foreign_order_id,'48afbc56-69c4-59b1-99a9-f36296934a88',sha256('foreign-customer'::bytea),
   current_setting('test.orders_foreign_customer')::uuid,NULL,'USD',0,'{}',lines,'[]');
  RAISE EXCEPTION 'ORDERS_GATE cross-tenant customer FK absent';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 IF EXISTS(SELECT FROM app.orders WHERE app.orders.id=foreign_order_id)
  OR EXISTS(SELECT FROM app.order_lines WHERE order_id=foreign_order_id)
  OR EXISTS(SELECT FROM app.outbox_events WHERE payload->>'order_id'=foreign_order_id::text) THEN
  RAISE EXCEPTION 'ORDERS_GATE rejected customer FK left partial snapshot';
 END IF;
END $customer_orders$;
RESET ROLE;
DO $customer_snapshot$
BEGIN
 BEGIN
  -- The replacement is another existing customer in the same tenant: neither
  -- XOR nor FK can hide a missing immutable-snapshot trigger.
  UPDATE app.orders SET customer_id=current_setting('test.orders_alternate_customer')::uuid
   WHERE id=current_setting('test.orders_customer_order')::uuid;
  RAISE EXCEPTION 'ORDERS_GATE immutable customer snapshot absent';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $customer_snapshot$;
ROLLBACK;
\echo 'ORDERS_GATE PASS snapshot, ownership, tenant, state, inventory and outbox'
