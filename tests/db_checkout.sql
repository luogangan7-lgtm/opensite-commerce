-- Responsibility: real DB checkout receipt/fence/tenant/grant witnesses; rollback.
-- Dependencies: checkout, Orders, Inventory; called by check_pg runner, not a mock.
\set ON_ERROR_STOP on
BEGIN;
-- Baseline RED proves the real checkout schema is required, not an optional test.
SELECT count(*) FROM app.checkout_sessions;
INSERT INTO app.tenants(name) VALUES('checkout-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('checkout-B') RETURNING id AS other_t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','checkout') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'product','checkout-sku','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('test.checkout_t',:'t',true),set_config('test.checkout_other_t',:'other_t',true),set_config('test.checkout_sku',:'sku',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',30,'checkout receipt',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $checkout$
DECLARE t uuid:=current_setting('test.checkout_t')::uuid; sku uuid:=current_setting('test.checkout_sku')::uuid;
 oid uuid:=uuidv7(); cid uuid:=uuidv7(); r app.inventory_reservations; o app.orders; session app.checkout_sessions;
BEGIN
 r:=app.inventory_reserve(sku,2,'order',oid,'2498c214-5ccf-58fe-bc90-65e675225d46',900);
 o:=app.orders_create(oid,'476f57f7-851d-5cf0-b7f4-e35cda3f5007',sha256('order'::bytea),NULL,'{"email":"checkout@example.invalid"}','USD',20,'{}',
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','checkout-sku','quantity',2,'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',10,'line_total_minor',210,'reservation_id',r.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]');
 INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
 VALUES(t,cid,'guest',repeat('a',64),'a066cb64-37a0-5474-bc5c-6f7467d8db63',sha256('start'::bytea),uuidv7(),1,oid,'{}','USD',230,200,10,20,1) RETURNING * INTO session;
 BEGIN
  INSERT INTO app.checkout_sessions SELECT * FROM app.checkout_sessions WHERE id=cid;
  RAISE EXCEPTION 'CHECKOUT_GATE duplicate actor/key accepted';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  UPDATE app.checkout_sessions SET amount_minor=1 WHERE id=cid;
  RAISE EXCEPTION 'CHECKOUT_GATE runtime rewrote payable';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  DELETE FROM app.checkout_sessions WHERE id=cid;
  RAISE EXCEPTION 'CHECKOUT_GATE runtime deleted receipt';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 UPDATE app.checkout_sessions SET phase='ready',provider_ref='fake_intent_witness',start_result='{"state":"ready"}' WHERE id=cid;
 INSERT INTO app.checkout_attempts(tenant_id,event_kind,event_key,checkout_id,request_hash) VALUES(t,'payment','b73e05a6-f90d-5de9-9db9-b4b5e6b2df74',cid,sha256('event'::bytea));
 UPDATE app.checkout_sessions SET phase='confirming',claim_kind='payment',claim_key='b73e05a6-f90d-5de9-9db9-b4b5e6b2df74' WHERE id=cid;
 BEGIN
  UPDATE app.checkout_sessions SET phase='cancelling',claim_kind='cancel',claim_key='4342706a-268f-56aa-a9c4-ce2b9bd70447' WHERE id=cid;
  RAISE EXCEPTION 'CHECKOUT_GATE opposing winner overwrote confirm fence';
 EXCEPTION WHEN check_violation THEN NULL; END;
 UPDATE app.checkout_sessions SET phase='paid',final_result='{"state":"paid"}' WHERE id=cid;
 UPDATE app.checkout_attempts SET result='{"state":"paid"}' WHERE event_key='b73e05a6-f90d-5de9-9db9-b4b5e6b2df74';
 BEGIN
  UPDATE app.checkout_attempts SET result='{"state":"cancelled"}' WHERE event_key='b73e05a6-f90d-5de9-9db9-b4b5e6b2df74';
  RAISE EXCEPTION 'CHECKOUT_GATE event result rewritten';
 EXCEPTION WHEN check_violation THEN NULL; END;
 PERFORM set_config('test.checkout_id',cid::text,true);
 PERFORM set_config('app.tenant_id',current_setting('test.checkout_other_t'),true);
 IF EXISTS(SELECT FROM app.checkout_sessions WHERE id=cid) OR EXISTS(SELECT FROM app.checkout_attempts WHERE checkout_id=cid) THEN RAISE EXCEPTION 'CHECKOUT_GATE tenant leaked receipt'; END IF;
 BEGIN
  INSERT INTO app.checkout_attempts(tenant_id,event_kind,event_key,checkout_id,request_hash) VALUES(current_setting('test.checkout_other_t')::uuid,'payment','5e292400-2e01-5a41-8801-7c6a516eaaf0',cid,sha256('foreign'::bytea));
  RAISE EXCEPTION 'CHECKOUT_GATE cross tenant receipt FK accepted';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 IF EXISTS(SELECT FROM app.checkout_sessions) OR EXISTS(SELECT FROM app.checkout_attempts) THEN RAISE EXCEPTION 'CHECKOUT_GATE missing tenant leaked'; END IF;
END $checkout$;
RESET ROLE;
DO $guard$
BEGIN
 BEGIN
  UPDATE app.checkout_sessions SET payable_version=99 WHERE id=current_setting('test.checkout_id')::uuid;
  RAISE EXCEPTION 'CHECKOUT_GATE trigger allowed snapshot rewrite';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  UPDATE app.checkout_sessions SET phase='ready' WHERE id=current_setting('test.checkout_id')::uuid;
  RAISE EXCEPTION 'CHECKOUT_GATE terminal reset accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  DELETE FROM app.checkout_attempts WHERE checkout_id=current_setting('test.checkout_id')::uuid;
  RAISE EXCEPTION 'CHECKOUT_GATE trigger allowed event deletion';
 EXCEPTION WHEN check_violation THEN NULL; END;
END $guard$;
ROLLBACK;
SELECT 'checkout SQL PASS' AS result;
