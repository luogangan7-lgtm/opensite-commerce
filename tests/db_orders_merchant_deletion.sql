-- Responsibility: MDEL-03 real-PG obligation, exact scrub and retained FK proof.
-- Dependencies: all forward migrations and real owner capabilities, CI only.
-- Invariants: quantities over UI states, exact tenant/epoch authority, no PII.
\set ON_ERROR_STOP on
BEGIN;
DO $$ BEGIN
 IF to_regprocedure('app.orders_merchant_obligations(uuid)') IS NULL
 OR to_regprocedure('app.orders_erase_merchant(uuid,uuid,bigint)') IS NULL THEN
  RAISE EXCEPTION 'MDEL03_OWNER_PORTS_REQUIRED'; END IF;
END $$;
\i tests/fixtures/mdel03_orders.sql
SELECT pg_temp.mdel03_fixture() AS guest \gset
SELECT pg_temp.mdel03_fixture(true) AS registered \gset
SELECT set_config('test.mdel03.guest',:'guest',true),set_config('test.mdel03.registered',:'registered',true);
DO $test$
DECLARE f jsonb:=current_setting('test.mdel03.guest')::jsonb; b jsonb:=current_setting('test.mdel03.registered')::jsonb;
 t uuid:=(f->>'t')::uuid; o uuid:=(f->>'o')::uuid; l uuid:=(f->>'l')::uuid;
 req uuid:=uuidv7(); v bigint; before_o jsonb; before_l jsonb; before_a jsonb; other_before jsonb; n bigint;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 IF (SELECT count FROM app.orders_merchant_obligations(t) LIMIT 1)<>0 THEN RAISE EXCEPTION 'MDEL03_PENDING_NOT_PAID'; END IF;
 PERFORM app.orders_transition(o,'paid','mdel03-pay',0);
 IF (SELECT count FROM app.orders_merchant_obligations(t) LIMIT 1)<>1 THEN RAISE EXCEPTION 'MDEL03_PAID_UNFULFILLED'; END IF;
 SELECT version INTO v FROM app.orders WHERE tenant_id=t AND id=o;
 PERFORM app.fulfillment_create_shipment(o,v,uuidv4()::text,'manual','mdel03-canary-tracking',
  jsonb_build_array(jsonb_build_object('order_line_id',l,'quantity',1)));
 IF (SELECT count FROM app.orders_merchant_obligations(t) LIMIT 1)<>1 THEN RAISE EXCEPTION 'MDEL03_PARTIAL_QUANTITY'; END IF;
 -- A full monetary refund is not a quantity witness: the remaining item blocks.
 PERFORM app.orders_transition(o,'refunded','mdel03-refund',250);
 IF (SELECT count FROM app.orders_merchant_obligations(t) LIMIT 1)<>1 THEN RAISE EXCEPTION 'MDEL03_REFUNDED_QUANTITY'; END IF;
 SELECT version INTO v FROM app.orders WHERE tenant_id=t AND id=o;
 PERFORM app.fulfillment_create_shipment(o,v,uuidv4()::text,'manual','mdel03-canary-tracking-2',
  jsonb_build_array(jsonb_build_object('order_line_id',l,'quantity',1)));
 IF (SELECT count FROM app.orders_merchant_obligations(t) LIMIT 1)<>0 THEN RAISE EXCEPTION 'MDEL03_FULFILLED_BOUNDARY'; END IF;
 BEGIN
  PERFORM app.orders_merchant_obligations((b->>'t')::uuid);
  RAISE EXCEPTION 'MDEL03_CROSS_TENANT_OBLIGATION';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 SELECT to_jsonb(x) INTO before_o FROM app.orders x WHERE tenant_id=t AND id=o;
 SELECT to_jsonb(x) INTO before_l FROM app.order_lines x WHERE tenant_id=t AND id=l;
 SELECT to_jsonb(x) INTO before_a FROM app.order_adjustments x WHERE tenant_id=t AND id=(f->>'a')::uuid;
 SELECT to_jsonb(x) INTO other_before FROM app.order_lines x WHERE tenant_id=(b->>'t')::uuid AND id=(b->>'l')::uuid;
 BEGIN
  PERFORM app.orders_erase_merchant(t,req,7);
  RAISE EXCEPTION 'MDEL03_ACTIVE_SCRUB_DENIED';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing',epoch=7,request_id=req,requester_id=uuidv7(),
  requested_at=clock_timestamp(),execute_after=clock_timestamp() WHERE tenant_id=t;
 BEGIN
  PERFORM app.orders_erase_merchant(t,req,6);
  RAISE EXCEPTION 'MDEL03_STALE_EPOCH';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 n:=app.orders_erase_merchant(t,req,7);
 IF n<>3 THEN RAISE EXCEPTION 'MDEL03_COUNTS'; END IF;
 IF EXISTS(SELECT FROM app.orders x WHERE tenant_id=t AND (
  to_jsonb(x)-ARRAY['guest_contact','shipping_snapshot'] IS DISTINCT FROM before_o-ARRAY['guest_contact','shipping_snapshot']
  OR guest_contact IS DISTINCT FROM jsonb_build_object('email','erased-'||o::text||'@invalid.test','erased',true)
  OR shipping_snapshot IS DISTINCT FROM jsonb_build_object('erased',true,'amount_minor',20,'currency','USD'))) THEN
  RAISE EXCEPTION 'MDEL03_MONEY_ID_WITNESS'; END IF;
 IF EXISTS(SELECT FROM app.order_lines x WHERE tenant_id=t AND (
  to_jsonb(x)-ARRAY['sku','tax_snapshot'] IS DISTINCT FROM before_l-ARRAY['sku','tax_snapshot']
  OR sku<>'[erased]' OR tax_snapshot<>'{"erased":true}'::jsonb)) THEN RAISE EXCEPTION 'MDEL03_DERIVED_SCRUB'; END IF;
 IF EXISTS(SELECT FROM app.order_adjustments x WHERE tenant_id=t AND (
  to_jsonb(x)-'reason' IS DISTINCT FROM before_a-'reason' OR reason<>'[erased]')) THEN RAISE EXCEPTION 'MDEL03_ADJUSTMENT_SCRUB'; END IF;
 IF (SELECT to_jsonb(x) FROM app.order_lines x WHERE tenant_id=(b->>'t')::uuid AND id=(b->>'l')::uuid)
  IS DISTINCT FROM other_before THEN RAISE EXCEPTION 'MDEL03_ERASE_TENANT_PREDICATE'; END IF;
 IF app.orders_erase_merchant(t,req,7)<>3 THEN RAISE EXCEPTION 'MDEL03_REPLAY'; END IF;
 -- Registered orders keep SQL NULL guest_contact and their customer FK.
 t:=(b->>'t')::uuid; PERFORM set_config('app.tenant_id',t::text,true);
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing',epoch=7,request_id=req,requester_id=uuidv7(),
  requested_at=clock_timestamp(),execute_after=clock_timestamp() WHERE tenant_id=t;
 PERFORM app.orders_erase_merchant(t,req,7);
 IF NOT EXISTS(SELECT FROM app.orders WHERE tenant_id=t AND id=(b->>'o')::uuid AND guest_contact IS NULL
  AND customer_id=(b->>'c')::uuid) THEN RAISE EXCEPTION 'MDEL03_REGISTERED_FK_NULL'; END IF;
END $test$;
SET LOCAL ROLE app_runtime;
DO $$ BEGIN
 BEGIN
  PERFORM app.orders_erase_merchant((current_setting('test.mdel03.guest')::jsonb->>'t')::uuid,uuidv7(),7);
  RAISE EXCEPTION 'MDEL03_RUNTIME_ERASE_GRANT';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN
  PERFORM app.orders_merchant_obligations((current_setting('test.mdel03.guest')::jsonb->>'t')::uuid);
  RAISE EXCEPTION 'MDEL03_MISSING_GUC';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $$;
ROLLBACK;
SELECT 'PASS: MDEL03 quantities, exact scrub, financial witnesses, tenant/epoch isolation';
