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
 IF (app.orders_merchant_obligations(t)->>'count')::bigint<>0 THEN RAISE EXCEPTION 'MDEL03_PENDING_NOT_PAID'; END IF;
 PERFORM app.orders_transition(o,'paid','mdel03-pay',0);
 IF (app.orders_merchant_obligations(t)->>'count')::bigint<>1 THEN RAISE EXCEPTION 'MDEL03_PAID_UNFULFILLED'; END IF;
 SELECT version INTO v FROM app.orders WHERE tenant_id=t AND id=o;
 PERFORM app.fulfillment_create_shipment(o,v,uuidv4()::text,'manual','mdel03-canary-tracking',
  jsonb_build_array(jsonb_build_object('order_line_id',l,'quantity',1)));
 IF (app.orders_merchant_obligations(t)->>'count')::bigint<>1 THEN RAISE EXCEPTION 'MDEL03_PARTIAL_QUANTITY'; END IF;
 -- A full monetary refund is not a quantity witness: the remaining item blocks.
 BEGIN
  PERFORM app.orders_transition(o,'refunded','mdel03-refund',250);
  IF (app.orders_merchant_obligations(t)->>'count')::bigint<>1 THEN RAISE EXCEPTION 'MDEL03_REFUNDED_QUANTITY'; END IF;
  -- A refunded order cannot ship; restore the partial state for the boundary.
  RAISE EXCEPTION 'restore refund witness' USING ERRCODE='P9003';
 EXCEPTION WHEN SQLSTATE 'P9003' THEN NULL; END;
 SELECT version INTO v FROM app.orders WHERE tenant_id=t AND id=o;
 PERFORM app.fulfillment_create_shipment(o,v,uuidv4()::text,'manual','mdel03-canary-tracking-2',
  jsonb_build_array(jsonb_build_object('order_line_id',l,'quantity',1)));
 IF (app.orders_merchant_obligations(t)->>'count')::bigint<>0 THEN RAISE EXCEPTION 'MDEL03_FULFILLED_BOUNDARY'; END IF;
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
SET LOCAL ROLE app_orders_privacy_executor;
DO $$ DECLARE t uuid:=(current_setting('test.mdel03.guest')::jsonb->>'t')::uuid; BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 BEGIN
  UPDATE app.orders SET shipping_snapshot='{}' WHERE tenant_id=t;
  RAISE EXCEPTION 'MDEL03_ARBITRARY_HEADER_SCRUB';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'MDEL03_EXACT_SCRUB_ONLY' THEN RAISE; END IF;
 END;
 BEGIN
  UPDATE app.order_lines SET sku='unapproved' WHERE tenant_id=t;
  RAISE EXCEPTION 'MDEL03_ARBITRARY_LINE_SCRUB';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'MDEL03_EXACT_SCRUB_ONLY' THEN RAISE; END IF;
 END;
 BEGIN
  UPDATE app.order_adjustments SET reason='unapproved' WHERE tenant_id=t;
  RAISE EXCEPTION 'MDEL03_ARBITRARY_ADJUSTMENT_SCRUB';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'MDEL03_EXACT_SCRUB_ONLY' THEN RAISE; END IF;
 END;
 BEGIN
  UPDATE app.orders SET total_minor=0 WHERE tenant_id=t;
  RAISE EXCEPTION 'MDEL03_MONEY_WRITE_GRANT';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $$;
RESET ROLE;
-- Every write prefix rejects ordinary business commands during cooling-off;
-- verified financial completion of an existing pending order can still drain.
SELECT pg_temp.mdel03_fixture() AS cooling \gset
SELECT set_config('test.mdel03.cooling',:'cooling',true);
DO $$ DECLARE f jsonb:=current_setting('test.mdel03.cooling')::jsonb; t uuid:=(f->>'t')::uuid; BEGIN
 UPDATE app.tenant_lifecycles SET state='cooling_off',phase='preconditions',request_id=uuidv7(),requester_id=uuidv7(),
  requested_at=clock_timestamp(),execute_after=clock_timestamp()+interval '720 hours' WHERE tenant_id=t;
 BEGIN
  PERFORM app.orders_check_creation(uuidv7(),uuidv4()::text,sha256('blocked'::bytea));
  RAISE EXCEPTION 'MDEL03_COOLING_NEW_WRITE';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'tenant deletion write fence' THEN RAISE; END IF;
 END;
 PERFORM app.orders_transition((f->>'o')::uuid,'paid','cooling-completion',0);
 IF (app.orders_merchant_obligations(t)->>'count')::bigint<>1 THEN RAISE EXCEPTION 'MDEL03_COOLING_DRAIN_WITNESS'; END IF;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing' WHERE tenant_id=t;
 BEGIN
  PERFORM app.orders_transition((f->>'o')::uuid,'paid','cooling-completion',0);
  RAISE EXCEPTION 'MDEL03_EXECUTING_LATE_COMPLETION';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'tenant deletion drain fence' THEN RAISE; END IF;
 END;
END $$;
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
