-- Responsibility: witness H-2-C owner fences and order key preflight on real PG.
-- Dependencies: production orders/inventory commands; called by check_pg/upgrade.
-- Invariants: same tenant is insufficient authority; denied commands change no rows.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('h2c-guards') RETURNING id AS t \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','h2c') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','H2C','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('test.h2c_sku',:'sku',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',10,'h2c stock',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $gate$
DECLARE r app.inventory_reservations; owner_a uuid:=uuidv7(); owner_b uuid:=uuidv7(); kind text; command text;
 before_movements bigint; before_orders bigint; before_reserved bigint; before_on_hand bigint;
BEGIN
 r:=app.inventory_reserve(current_setting('test.h2c_sku')::uuid,2,'checkout',owner_a,'h2c-owner',900);
 SELECT count(*) INTO before_movements FROM app.inventory_movements;
 SELECT count(*) INTO before_orders FROM app.orders;
 SELECT reserved,on_hand INTO before_reserved,before_on_hand FROM app.stock_levels;
 FOREACH command IN ARRAY ARRAY['inventory_release_non_order','inventory_commit_non_order'] LOOP
  BEGIN
   EXECUTE format('SELECT app.%I($1,$2,$3)',command) USING r.id,'checkout',owner_b;
   RAISE EXCEPTION 'H2C_OWNER_GATE foreign checkout owner accepted %',command;
  EXCEPTION WHEN insufficient_privilege THEN
   IF SQLERRM<>'INVENTORY_RESERVATION_OWNER_MISMATCH' THEN RAISE; END IF;
  END;
  BEGIN
   EXECUTE format('SELECT app.%I($1,$2,$3)',command) USING r.id,'Checkout',owner_a;
   RAISE EXCEPTION 'H2C_OWNER_GATE owner type case mismatch accepted %',command;
  EXCEPTION WHEN insufficient_privilege THEN
   IF SQLERRM<>'INVENTORY_RESERVATION_OWNER_MISMATCH' THEN RAISE; END IF;
  END;
 END LOOP;
 IF (SELECT state FROM app.inventory_reservations WHERE id=r.id)<>'active'
 OR (SELECT count(*) FROM app.inventory_movements)<>before_movements
 OR (SELECT count(*) FROM app.orders)<>before_orders
 OR (SELECT reserved FROM app.stock_levels)<>before_reserved
 OR (SELECT on_hand FROM app.stock_levels)<>before_on_hand THEN
  RAISE EXCEPTION 'H2C_OWNER_GATE denied command had side effects';
 END IF;
 PERFORM app.inventory_release_non_order(r.id,'checkout',owner_a);
 PERFORM app.inventory_release_non_order(r.id,'checkout',owner_a);
 IF (SELECT count(*) FROM app.inventory_movements)<>before_movements+1 THEN
  RAISE EXCEPTION 'H2C_OWNER_GATE owned replay duplicated release'; END IF;
 FOREACH kind IN ARRAY ARRAY['order','Order','ORDER'] LOOP
  r:=app.inventory_reserve(current_setting('test.h2c_sku')::uuid,1,kind,owner_a,'h2c-case-'||kind,900);
  FOREACH command IN ARRAY ARRAY['inventory_release_non_order','inventory_commit_non_order'] LOOP
   BEGIN
    EXECUTE format('SELECT app.%I($1,$2,$3)',command) USING r.id,kind,owner_a;
    RAISE EXCEPTION 'H2C_CASE_GATE order owner case bypass % %',kind,command;
   EXCEPTION WHEN insufficient_privilege THEN
    IF SQLERRM<>'INVENTORY_ORDER_OWNED_RESERVATION' THEN RAISE; END IF;
   END;
  END LOOP;
 END LOOP;
 -- Invalid tax categories fail before reservation lookup and leave no snapshot.
 FOREACH kind IN ARRAY ARRAY['', 'Standard', '_standard', '1standard', 'bad-code', 'é', repeat('a',65)] LOOP
  BEGIN
   PERFORM app.orders_create(uuidv7(),'h2c-tax-'||kind,sha256(convert_to(kind,'UTF8')),NULL,
    '{"email":"gate@example.test"}'::jsonb,'USD',0,'{}'::jsonb,
    jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',current_setting('test.h2c_sku')::uuid,'sku','H2C','quantity',1,
     'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',uuidv7(),
     'tax_snapshot',jsonb_build_object('category',kind))),'[]');
   RAISE EXCEPTION 'H2C_TAX_GATE invalid category accepted %',kind;
  EXCEPTION WHEN invalid_parameter_value THEN
   IF SQLERRM<>'ORDER_INVALID_LINE' THEN RAISE; END IF;
  END;
 END LOOP;
 -- A category must be a JSON string, not a scalar that ->> coerces to text.
 BEGIN
  PERFORM app.orders_create(uuidv7(),'h2c-tax-json',sha256('json'::bytea),NULL,
   '{"email":"gate@example.test"}'::jsonb,'USD',0,'{}'::jsonb,
   jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',current_setting('test.h2c_sku')::uuid,'sku','H2C','quantity',1,
    'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',uuidv7(),
    'tax_snapshot','{"category":true}'::jsonb)),'[]');
  RAISE EXCEPTION 'H2C_TAX_GATE non-string category accepted';
 EXCEPTION WHEN invalid_parameter_value THEN
  IF SQLERRM<>'ORDER_INVALID_LINE' THEN RAISE; END IF;
 END;
 -- A null reservation id is malformed input, not a missing reservation.
 FOREACH command IN ARRAY ARRAY['inventory_release_non_order','inventory_commit_non_order'] LOOP
  BEGIN
   EXECUTE format('SELECT app.%I(NULL::uuid,$1,$2)',command) USING 'checkout',owner_a;
   RAISE EXCEPTION 'H2C_NULL_GATE null reservation accepted %',command;
  EXCEPTION WHEN invalid_parameter_value THEN
   IF SQLERRM<>'INVENTORY_INVALID_ARGUMENT' THEN RAISE; END IF;
  END;
 END LOOP;
 -- One lock order: the preflight takes only a creation-key lock, never the tenant
 -- number lock (held later by orders_create after stock locks).
 PERFORM app.orders_check_creation(uuidv7(),'h2c-lock-key',sha256('lock'::bytea));
 IF NOT EXISTS(SELECT FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted
    AND ((classid::bigint<<32)|objid::bigint)=hashtextextended('orders-key:'||current_setting('app.tenant_id')||':h2c-lock-key',0))
 OR EXISTS(SELECT FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid()
    AND ((classid::bigint<<32)|objid::bigint)=hashtextextended('orders-number:'||current_setting('app.tenant_id'),0)) THEN
  RAISE EXCEPTION 'H2C_LOCK_GATE preflight must hold the key lock and not the tenant number lock';
 END IF;
 IF to_regprocedure('app.inventory_release_non_order(uuid)') IS NOT NULL
 OR to_regprocedure('app.inventory_commit_non_order(uuid)') IS NOT NULL THEN
  RAISE EXCEPTION 'H2C_OWNER_GATE unbound legacy command remains'; END IF;
END $gate$;
ROLLBACK;
SELECT 'PASS: H2C_OWNER_GATE/H2C_CASE_GATE exact owner binding and stock/order conservation' AS result;
