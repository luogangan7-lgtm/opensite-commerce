-- Responsibility: prove holder authority, not readable tenant-local identifiers.
-- Depends on controlled inventory commands; executed by check_pg on real PG.
-- Invariants: observing/replaying rows cannot finish another holder's reservation.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('c20b-owner') RETURNING id AS tenant \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'tenant','c20b') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(:'tenant',:'product','C20B','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'tenant',true),set_config('test.c20b_sku',:'sku',true);
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',10,'c20b stock',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $gate$
DECLARE r app.inventory_reservations; seen app.inventory_reservations; command text;
 bearer bytea:=sha256(convert_to(uuidv4()::text||uuidv4()::text||uuidv4()::text,'UTF8'));
 other bytea:=sha256(convert_to(uuidv4()::text||uuidv4()::text||uuidv4()::text,'UTF8'));
 owner uuid:=uuidv7(); before_moves bigint; h bytea;
BEGIN
 -- The tests-only commit also runs on the vulnerable API. Once removed, the
 -- identical regression uses the capability API and the observed stored hash.
 IF to_regprocedure('app.inventory_release_non_order(uuid,text,uuid)') IS NOT NULL THEN
  r:=app.inventory_reserve(current_setting('test.c20b_sku')::uuid,1,'checkout',owner,'c20b-replay',900);
 ELSE
  EXECUTE 'SELECT * FROM app.inventory_reserve_non_order($1,1,''checkout'',$2,$3,900,$4)'
   INTO r USING current_setting('test.c20b_sku')::uuid,owner,'c20b-replay',bearer;
 END IF;
 SELECT * INTO STRICT seen FROM app.inventory_reservations WHERE id=r.id;
 IF NOT EXISTS(SELECT FROM app.inventory_movements WHERE reservation_id=r.id
   AND owner_type=seen.owner_type AND owner_id=seen.owner_id) THEN
  RAISE EXCEPTION 'C20B_GATE owner observation fixture missing'; END IF;
 SELECT count(*) INTO before_moves FROM app.inventory_movements;
 FOREACH command IN ARRAY ARRAY['inventory_release_non_order','inventory_commit_non_order'] LOOP
  BEGIN
   IF to_regprocedure('app.inventory_release_non_order(uuid,text,uuid)') IS NOT NULL THEN
    EXECUTE format('SELECT app.%I($1,$2,$3)',command) USING seen.id,seen.owner_type,seen.owner_id;
   ELSE
    EXECUTE 'SELECT holder_hash FROM app.inventory_reservations WHERE id=$1' INTO h USING seen.id;
    EXECUTE format('SELECT app.%I($1,$2,$3,$4)',command) USING seen.id,seen.owner_type,seen.owner_id,h;
   END IF;
   RAISE EXCEPTION 'C20B_GATE observed owner/id/hash replay accepted %',command;
  EXCEPTION WHEN insufficient_privilege THEN NULL;
  END;
 END LOOP;
 IF (SELECT state FROM app.inventory_reservations WHERE id=r.id)<>'active'
 OR (SELECT count(*) FROM app.inventory_movements)<>before_moves THEN
  RAISE EXCEPTION 'C20B_GATE denied replay mutated inventory'; END IF;
 -- A replay of reserve with observed identifiers cannot mint another bearer.
 BEGIN
  EXECUTE 'SELECT * FROM app.inventory_reserve_non_order($1,1,$2,$3,$4,900,$5)'
   USING seen.sku_id,seen.owner_type,seen.owner_id,'c20b-replay',other;
  RAISE EXCEPTION 'C20B_GATE reserve replay replaced holder';
 EXCEPTION WHEN invalid_parameter_value THEN
  IF SQLERRM<>'INVENTORY_IDEMPOTENCY_KEY_REUSED' THEN RAISE; END IF;
 END;
 EXECUTE 'SELECT app.inventory_release_non_order($1,$2,$3,$4)' USING r.id,r.owner_type,r.owner_id,bearer;
 EXECUTE 'SELECT app.inventory_release_non_order($1,$2,$3,$4)' USING r.id,r.owner_type,r.owner_id,bearer;
 BEGIN
  EXECUTE 'SELECT app.inventory_release_non_order($1,$2,$3,$4)' USING r.id,r.owner_type,r.owner_id,other;
  RAISE EXCEPTION 'C20B_GATE terminal replay skipped holder check';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
 IF (SELECT count(*) FROM app.inventory_movements)<>before_moves+1 THEN
  RAISE EXCEPTION 'C20B_GATE holder replay duplicated movement'; END IF;
 BEGIN
  PERFORM app.inventory_expire_due(100);
  RAISE EXCEPTION 'C20B_GATE runtime can sweep foreign holds';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
 IF to_regprocedure('app.inventory_release_non_order(uuid,text,uuid)') IS NOT NULL
 OR to_regprocedure('app.inventory_commit_non_order(uuid,text,uuid)') IS NOT NULL THEN
  RAISE EXCEPTION 'C20B_GATE legacy owner-only terminal remains'; END IF;
END $gate$;
ROLLBACK;
SELECT 'PASS: C20B_GATE observed identifiers/hash rejected; holder replay authorized; runtime sweep denied' AS result;
