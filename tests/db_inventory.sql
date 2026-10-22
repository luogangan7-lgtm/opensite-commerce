-- C-11a public inventory contract. Real PG18, expendable DB; all fixtures rollback.
-- Depends on catalog SKUs and inventory functions; called by check_pg.sh.
-- Invariants: bounded counters, one reservation per key, terminal transitions,
-- tenant isolation, trusted-only adjustment and immutable matching movements.
\set ON_ERROR_STOP on
BEGIN;
-- C-20b: runtime finishes non-order reservations only via holder-fenced
-- *_non_order commands (same 012 terminal algorithm); order-owned holds are
-- finished by orders_transition. No temporary grants are used.
INSERT INTO app.tenants(name) VALUES('inventory-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('inventory-B') RETURNING id AS foreign_t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','inventory') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'t',:'p','inventory-sku','default') RETURNING id AS sku \gset
SELECT set_config('test.inventory_t',:'t',true),set_config('test.inventory_sku',:'sku',true);
SELECT set_config('test.inventory_foreign_t',:'foreign_t',true);
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',5,'initial receipt',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $contract$
DECLARE t uuid:=current_setting('test.inventory_t')::uuid;
 s uuid:=current_setting('test.inventory_sku')::uuid; owner_id uuid:=uuidv7();
 r app.inventory_reservations; replay app.inventory_reservations; n bigint;
 bearer bytea:=sha256(convert_to(uuidv4()::text||uuidv4()::text||uuidv4()::text,'UTF8'));
BEGIN
 r:=app.inventory_reserve_non_order(s,3,'checkout',owner_id,'inventory-key',900,bearer);
 replay:=app.inventory_reserve_non_order(s,3,'checkout',owner_id,'inventory-key',900,bearer);
 IF r.id<>replay.id OR r.expires_at<>replay.expires_at THEN
  RAISE EXCEPTION 'INVENTORY_GATE replay changed reservation or TTL';
 END IF;
 SELECT count(*) INTO n FROM app.inventory_movements WHERE tenant_id=t;
 IF n<>2 OR (SELECT reserved FROM app.stock_levels WHERE tenant_id=t)<>3 THEN
  RAISE EXCEPTION 'INVENTORY_GATE replay duplicated movement/reserved';
 END IF;
 BEGIN
  PERFORM app.inventory_reserve_non_order(s,4,'checkout',owner_id,'inventory-key',900,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE changed-key-request accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  PERFORM app.inventory_reserve_non_order(s,3,'checkout',owner_id,'inventory-key',901,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE changed-key-TTL accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  PERFORM app.inventory_reserve_non_order(s,3,'checkout',owner_id,'last-too-many',900,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE oversell accepted';
 EXCEPTION WHEN raise_exception THEN
  IF SQLERRM<>'INVENTORY_INSUFFICIENT_STOCK' THEN RAISE; END IF;
 END;
 BEGIN
  PERFORM app.inventory_adjust(s,100,'forged stock',uuidv7());
  RAISE EXCEPTION 'INVENTORY_GATE runtime adjusts stock';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.inventory_finish(r.id,'expired');
  RAISE EXCEPTION 'INVENTORY_GATE private finish executable';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.stock_levels SET reserved=0 WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE runtime bypasses stock function';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.inventory_movements SET reason='forged' WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE runtime mutates movement';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM app.inventory_reserve(s,0,'order',owner_id,'zero',900);
  RAISE EXCEPTION 'INVENTORY_GATE zero quantity accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 BEGIN
  PERFORM app.inventory_reserve(s,1,'order',owner_id,'zero-ttl',0);
  RAISE EXCEPTION 'INVENTORY_GATE zero TTL accepted';
 EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
 PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
 replay:=app.inventory_reserve_non_order(s,3,'checkout',owner_id,'inventory-key',900,bearer);
 IF replay.id<>r.id OR replay.state<>'released' OR replay.expires_at<>r.expires_at THEN
  RAISE EXCEPTION 'INVENTORY_GATE released key was reused or expiry extended';
 END IF;
 IF (SELECT reserved FROM app.stock_levels WHERE tenant_id=t)<>0
  OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t)<>3 THEN
  RAISE EXCEPTION 'INVENTORY_GATE release not exactly once';
 END IF;
 BEGIN
  PERFORM app.inventory_commit_non_order(r.id,r.owner_type,r.owner_id,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE released reservation committed';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 r:=app.inventory_reserve_non_order(s,2,'checkout',owner_id,'commit-key',900,bearer);
 PERFORM app.inventory_commit_non_order(r.id,r.owner_type,r.owner_id,bearer);
 PERFORM app.inventory_commit_non_order(r.id,r.owner_type,r.owner_id,bearer);
 replay:=app.inventory_reserve_non_order(s,2,'checkout',owner_id,'commit-key',900,bearer);
 IF replay.id<>r.id OR replay.state<>'committed' OR replay.expires_at<>r.expires_at THEN
  RAISE EXCEPTION 'INVENTORY_GATE committed key was reused or expiry extended';
 END IF;
 IF (SELECT on_hand FROM app.stock_levels WHERE tenant_id=t)<>3
  OR (SELECT reserved FROM app.stock_levels WHERE tenant_id=t)<>0
  OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t)<>5 THEN
  RAISE EXCEPTION 'INVENTORY_GATE commit not exactly once';
 END IF;
 BEGIN
  PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE committed reservation released';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Subtransaction: the order-owned probe hold is rolled back afterwards.
 BEGIN
  r:=app.inventory_reserve(s,1,'order',owner_id,'order-owned-key',900);
  BEGIN
   PERFORM app.inventory_commit_non_order(r.id,r.owner_type,r.owner_id,bearer);
   RAISE EXCEPTION 'INVENTORY_GATE runtime committed order-owned reservation';
  EXCEPTION WHEN insufficient_privilege THEN
   IF SQLERRM<>'INVENTORY_ORDER_OWNED_RESERVATION' THEN RAISE; END IF;
  END;
  BEGIN
   PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
   RAISE EXCEPTION 'INVENTORY_GATE runtime released order-owned reservation';
  EXCEPTION WHEN insufficient_privilege THEN
   IF SQLERRM<>'INVENTORY_ORDER_OWNED_RESERVATION' THEN RAISE; END IF;
  END;
  BEGIN
   PERFORM app.inventory_commit(r.id);
   RAISE EXCEPTION 'INVENTORY_GATE runtime has raw commit';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  RAISE EXCEPTION 'INVENTORY_PROBE_ROLLBACK';
 EXCEPTION WHEN raise_exception THEN
  IF SQLERRM<>'INVENTORY_PROBE_ROLLBACK' THEN RAISE; END IF;
 END;
 r:=app.inventory_reserve_non_order(s,1,'checkout',owner_id,'expire-key',900,bearer);
 PERFORM set_config('test.inventory_expire_id',r.id::text,true);
 -- Transaction-local fixture state; never a production token storage path.
 PERFORM set_config('test.inventory_expire_bearer',encode(bearer,'hex'),true);
 PERFORM set_config('app.tenant_id','',true);
 IF (SELECT count(*) FROM app.stock_levels)<>0 THEN
  RAISE EXCEPTION 'INVENTORY_GATE no-context row visible';
 END IF;
 BEGIN
  PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE missing tenant accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',t::text,true);
 PERFORM set_config('app.tenant_id','not-a-uuid',true);
 BEGIN
  PERFORM app.inventory_release_non_order(r.id,r.owner_type,r.owner_id,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE malformed tenant accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',t::text,true);
END $contract$;
RESET ROLE;
-- Bypass application privileges ONLY to inject an already-due test fixture.
UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second'
 WHERE id=current_setting('test.inventory_expire_id')::uuid;
SET LOCAL ROLE app_runtime;
DO $expiry$
DECLARE r uuid:=current_setting('test.inventory_expire_id')::uuid; n integer;
 original app.inventory_reservations; replay app.inventory_reservations; movements bigint;
 bearer bytea:=decode(current_setting('test.inventory_expire_bearer'),'hex');
BEGIN
 SELECT * INTO original FROM app.inventory_reservations WHERE id=r;
 BEGIN
  PERFORM app.inventory_commit_non_order(r,original.owner_type,original.owner_id,bearer);
  RAISE EXCEPTION 'INVENTORY_GATE expired reservation committed';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'INVENTORY_RESERVATION_EXPIRED' THEN RAISE; END IF;
 END;
 SELECT * INTO original FROM app.inventory_reservations WHERE id=r;
 BEGIN
  PERFORM app.inventory_expire_due(1);
  RAISE EXCEPTION 'INVENTORY_GATE runtime has expiry authority';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 SET LOCAL ROLE app_queue_worker;
 n:=app.inventory_expire_due(1);
 IF n<>1 OR app.inventory_expire_due(1)<>0 THEN
  RAISE EXCEPTION 'INVENTORY_GATE expiry not bounded/exactly once';
 END IF;
 SET LOCAL ROLE app_runtime;
 IF (SELECT reserved FROM app.stock_levels)<>0
  OR (SELECT on_hand FROM app.stock_levels)<>3
  OR (SELECT state FROM app.inventory_reservations WHERE id=r)<>'expired' THEN
  RAISE EXCEPTION 'INVENTORY_GATE expiration changed physical stock';
 END IF;
 SELECT count(*) INTO movements FROM app.inventory_movements;
 replay:=app.inventory_reserve_non_order(original.sku_id,original.quantity,original.owner_type,
  original.owner_id,original.idempotency_key,original.ttl_seconds,bearer);
 IF replay.id<>r OR replay.state<>'expired' OR replay.expires_at<>original.expires_at
  OR (SELECT count(*) FROM app.inventory_movements)<>movements
  OR (SELECT reserved FROM app.stock_levels)<>0 THEN
  RAISE EXCEPTION 'INVENTORY_GATE expired key reserved again or changed evidence';
 END IF;
END $expiry$;
RESET ROLE;
DO $bounds$
DECLARE t uuid:=current_setting('test.inventory_t')::uuid;
BEGIN
 BEGIN
  UPDATE app.stock_levels SET reserved=on_hand+1 WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE reserved exceeds physical stock';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  UPDATE app.stock_levels SET reserved=-1 WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE negative reserved accepted';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  UPDATE app.inventory_movements SET reason='changed' WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE movement mutated';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  DELETE FROM app.inventory_movements WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE movement deleted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  INSERT INTO app.stock_levels(tenant_id,warehouse_id,sku_id)
  SELECT current_setting('test.inventory_foreign_t')::uuid,warehouse_id,sku_id FROM app.stock_levels WHERE tenant_id=t;
  RAISE EXCEPTION 'INVENTORY_GATE foreign warehouse/SKU accepted';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $bounds$;
-- Tenant B cannot operate or even see a reservation owned by tenant A.
SELECT set_config('app.tenant_id',:'foreign_t',true);
SET LOCAL ROLE app_runtime;
DO $foreign$
BEGIN
 IF (SELECT count(*) FROM app.inventory_reservations)<>0 THEN
  RAISE EXCEPTION 'INVENTORY_GATE cross-tenant read';
 END IF;
 BEGIN
  PERFORM app.inventory_release_non_order(current_setting('test.inventory_expire_id')::uuid,'checkout',uuidv7(),
   decode(current_setting('test.inventory_expire_bearer'),'hex'));
  RAISE EXCEPTION 'INVENTORY_GATE cross-tenant operation';
 EXCEPTION WHEN no_data_found THEN NULL; END;
END $foreign$;
RESET ROLE;
DO $conservation$
BEGIN
 IF EXISTS(SELECT FROM app.stock_levels s WHERE s.reserved<>
  (SELECT coalesce(sum(quantity),0) FROM app.inventory_reservations r
   WHERE (r.tenant_id,r.warehouse_id,r.sku_id)=(s.tenant_id,s.warehouse_id,s.sku_id)
   AND r.state='active')) THEN
  RAISE EXCEPTION 'INVENTORY_GATE reserved differs from active reservations';
 END IF;
 IF EXISTS(SELECT FROM app.stock_levels s WHERE s.on_hand<>
  (SELECT coalesce(sum(on_hand_delta),0) FROM app.inventory_movements m
   WHERE (m.tenant_id,m.warehouse_id,m.sku_id)=(s.tenant_id,s.warehouse_id,s.sku_id))) THEN
  RAISE EXCEPTION 'INVENTORY_GATE stock differs from movement sum';
 END IF;
END $conservation$;
ROLLBACK;
SELECT 'PASS: inventory constraints/idempotency/transitions/immutable movements/tenant authority' AS result;
