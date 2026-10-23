-- stable-id: c19d2-projections
-- depends-on: c19d2-terminal
-- Responsibility: domain-owned compensation ports and independent posted-witness guards.
-- Dependencies: immutable Payments terminal receipt, Ledger witness, frozen domain snapshots.
-- Caller: Payments finalizer only; generic transitions retain all original restrictions.
ALTER TABLE app.returns DROP CONSTRAINT returns_status_check;
ALTER TABLE app.returns ADD CONSTRAINT returns_status_check CHECK(status IN('requested','received','refund_pending','refund_reconcile','refund_failed','refunded','completed','refund_compensated'));
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
 IF NEW.refunded_minor<OLD.refunded_minor THEN
  IF current_user<>'app_orders_executor' OR NOT EXISTS(
   SELECT FROM app.payment_refund_terminals c JOIN app.payment_refund_reversals w
    ON (w.tenant_id,w.reversal_journal_id)=(c.tenant_id,c.reversal_journal_id)
   JOIN app.ledger_transactions j ON (j.tenant_id,j.id)=(w.tenant_id,w.reversal_journal_id)
   WHERE c.tenant_id=NEW.tenant_id AND c.order_id=NEW.id AND j.status='posted'
    AND c.order_version=OLD.version AND NEW.version=OLD.version+1 AND NEW.sealed
    AND OLD.refunded_minor-NEW.refunded_minor=c.amount_minor AND NEW.state=c.order_target) THEN
   RAISE EXCEPTION 'C19D2_ORDER_WITNESS_REQUIRED' USING ERRCODE='23514'; END IF;
  RETURN NEW;
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
CREATE OR REPLACE FUNCTION app.fulfillment_header_guard() RETURNS trigger LANGUAGE plpgsql
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
  IF OLD.status='refunded' AND NEW.status='refund_compensated' THEN
   IF NOT EXISTS(SELECT FROM app.payment_refund_terminals c JOIN app.payment_refund_reversals w
     ON (w.tenant_id,w.reversal_journal_id)=(c.tenant_id,c.reversal_journal_id)
    JOIN app.ledger_transactions j ON (j.tenant_id,j.id)=(w.tenant_id,w.reversal_journal_id)
    WHERE c.tenant_id=NEW.tenant_id AND c.return_id=NEW.id AND j.status='posted'
     AND c.original_journal_id=OLD.ledger_transaction_id AND c.amount_minor=OLD.refund_minor AND NEW.order_version=c.order_version+1)
    OR (to_jsonb(NEW)-ARRAY['status','order_version']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','order_version']) THEN
    RAISE EXCEPTION 'C19D2_RETURN_WITNESS_REQUIRED' USING ERRCODE='23514'; END IF;
   RETURN NEW;
  END IF;
  -- Compile return-only fields only for return records; shipments use this guard too.
  IF OLD.status IS DISTINCT FROM NEW.status AND NOT(
   OLD.status='requested' AND NEW.status='received' OR OLD.status='received' AND NEW.status='refund_pending'
   OR OLD.status='refund_pending' AND NEW.status IN('refunded','refund_failed','refund_reconcile')
   OR OLD.status='refund_reconcile' AND NEW.status IN('refunded','refund_failed')
   OR OLD.status='refund_failed' AND NEW.status='refund_pending'
   OR OLD.status='received' AND NEW.status='completed' AND NEW.refund_minor=0) THEN
   RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION app.guard_checkout_refund() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.refunded_net_minor<>0 OR NEW.refunded_tax_minor<>0 OR NEW.refunded_shipping_minor<>0 THEN
   RAISE EXCEPTION 'refund starts zero' USING ERRCODE='23514'; END IF;
 ELSIF NEW.refunded_net_minor<OLD.refunded_net_minor OR NEW.refunded_tax_minor<OLD.refunded_tax_minor OR NEW.refunded_shipping_minor<OLD.refunded_shipping_minor THEN
  IF current_user<>'app_payments_service' OR NOT EXISTS(
   SELECT FROM app.payment_refund_terminals c JOIN app.payment_refund_reversals w
    ON (w.tenant_id,w.reversal_journal_id)=(c.tenant_id,c.reversal_journal_id)
   JOIN app.ledger_transactions j ON (j.tenant_id,j.id)=(w.tenant_id,w.reversal_journal_id)
   WHERE c.tenant_id=NEW.tenant_id AND c.order_id=NEW.order_id AND j.status='posted'
    AND c.checkout_net=OLD.refunded_net_minor AND c.checkout_tax=OLD.refunded_tax_minor AND c.checkout_shipping=OLD.refunded_shipping_minor
    AND (OLD.refunded_net_minor-NEW.refunded_net_minor,OLD.refunded_tax_minor-NEW.refunded_tax_minor,OLD.refunded_shipping_minor-NEW.refunded_shipping_minor)
      =(c.income_minor,c.tax_minor,c.shipping_minor)
    AND NEW.phase='paid' AND EXISTS(SELECT FROM app.orders o WHERE o.tenant_id=c.tenant_id AND o.id=c.order_id AND o.version=c.order_version+1)) THEN
   RAISE EXCEPTION 'C19D2_CHECKOUT_WITNESS_REQUIRED' USING ERRCODE='23514'; END IF;
 ELSIF (NEW.refunded_net_minor,NEW.refunded_tax_minor,NEW.refunded_shipping_minor) IS DISTINCT FROM
  (OLD.refunded_net_minor,OLD.refunded_tax_minor,OLD.refunded_shipping_minor) AND NEW.phase<>'paid' THEN
  RAISE EXCEPTION 'refund counters require capture' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
-- SQL ports accept only the posted reversal identity, never caller amounts or target states.
CREATE FUNCTION app.orders_compensate_refund(p_reversal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); c app.payment_refund_terminals;
BEGIN
 SELECT * INTO STRICT c FROM app.payment_refund_terminals WHERE tenant_id=t AND reversal_journal_id=p_reversal;
 IF EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND delivery_target='orders.events' AND dedup_key='compensate:'||p_reversal) THEN RETURN; END IF;
 PERFORM app.orders_refund_budget(c.order_id,'freeze',NULL,NULL,NULL,NULL,0);
 UPDATE app.orders SET refunded_minor=refunded_minor-c.amount_minor,state=c.order_target,version=version+1,updated_at=clock_timestamp()
 WHERE tenant_id=t AND id=c.order_id AND version=c.order_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'C19D2_ORDER_REVISION' USING ERRCODE='40001'; END IF;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'order.refund_compensated',jsonb_build_object('order_id',c.order_id,'original_journal_id',c.original_journal_id,
 'reversal_journal_id',p_reversal),'orders.events','compensate:'||p_reversal);
END $$;
ALTER FUNCTION app.orders_compensate_refund(uuid) OWNER TO app_orders_executor;
REVOKE ALL ON FUNCTION app.orders_compensate_refund(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.orders_compensate_refund(uuid) TO app_payments_service;
CREATE FUNCTION app.returns_compensate_refund(p_reversal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); c app.payment_refund_terminals;
BEGIN
 SELECT * INTO STRICT c FROM app.payment_refund_terminals WHERE tenant_id=t AND reversal_journal_id=p_reversal;
 IF c.return_id IS NULL THEN RETURN; END IF;
 UPDATE app.returns SET status='refund_compensated',order_version=c.order_version+1
 WHERE tenant_id=t AND id=c.return_id AND status='refunded' AND ledger_transaction_id=c.original_journal_id;
 IF NOT FOUND AND NOT EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND id=c.return_id AND status='refund_compensated') THEN
  RAISE EXCEPTION 'C19D2_RETURN_REVISION' USING ERRCODE='23514'; END IF;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(t,'return.refund_compensated',jsonb_build_object('return_id',c.return_id,'reversal_journal_id',p_reversal),
 'returns.events','compensate:'||p_reversal) ON CONFLICT DO NOTHING;
END $$;
ALTER FUNCTION app.returns_compensate_refund(uuid) OWNER TO app_fulfillment_executor;
REVOKE ALL ON FUNCTION app.returns_compensate_refund(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.returns_compensate_refund(uuid) TO app_payments_service;
GRANT SELECT ON app.payment_refund_terminals,app.payment_refund_reversals,app.ledger_transactions TO app_orders_executor,app_fulfillment_executor;
-- The closed Orders budget still treats compensated allocation as permanently sealed.
CREATE OR REPLACE FUNCTION app.orders_refund_budget(p_order uuid,p_phase text,p_return uuid,
 p_expected bigint,p_target text,p_event_key text,p_refund_minor bigint) RETURNS app.orders
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; r app.returns; e app.outbox_events;
 pending numeric; confirmed_rmas numeric; frozen numeric; legacy numeric; next_confirmed numeric;
BEGIN
 IF p_order IS NULL OR p_phase IS NULL OR p_phase NOT IN('freeze','reserve','lock','fail','complete','legacy')
  OR (p_expected IS NOT NULL AND p_expected<=0)
  OR (p_phase='legacy' AND (p_return IS NOT NULL OR p_target IS NULL OR p_target NOT IN('partially_refunded','refunded')
   OR p_event_key IS NULL OR btrim(p_event_key)='' OR length(p_event_key)>128 OR p_refund_minor IS NULL OR p_refund_minor<=0))
  OR (p_phase<>'legacy' AND (p_target IS NOT NULL OR p_event_key IS NOT NULL OR p_refund_minor IS DISTINCT FROM 0))
  OR (p_phase IN('reserve','lock','fail','complete') AND p_return IS NULL) THEN
  RAISE EXCEPTION 'ORDER_INVALID_REFUND' USING ERRCODE='22023'; END IF;
 SELECT * INTO o FROM app.orders WHERE tenant_id=t AND id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'ORDER_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_expected IS NOT NULL AND o.version<>p_expected THEN
  RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 -- REFUND_BUDGET_PARENT_FENCE: real MVCC write, even on exact event replay.
 -- Preserve the full replay snapshot; financial changes below use the DB clock.
 UPDATE app.orders SET updated_at=updated_at WHERE tenant_id=t AND id=p_order RETURNING * INTO o;
 PERFORM rr.id FROM app.returns rr WHERE rr.tenant_id=t AND rr.order_id=p_order ORDER BY rr.id FOR UPDATE;
 IF NOT o.sealed OR EXISTS(SELECT FROM app.returns rr WHERE rr.tenant_id=t AND rr.order_id=p_order
  AND (NOT rr.sealed OR rr.currency IS DISTINCT FROM o.currency OR rr.refund_minor<0
   OR rr.refund_minor::numeric IS DISTINCT FROM (SELECT coalesce(sum(rl.income_minor::numeric+rl.tax_minor+rl.shipping_minor),0)
     FROM app.return_lines rl WHERE rl.tenant_id=t AND rl.return_id=rr.id))) THEN
  RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
 SELECT coalesce(sum(rr.refund_minor) FILTER(WHERE rr.status IN('refund_pending','refund_reconcile')),0),
  coalesce(sum(rr.refund_minor) FILTER(WHERE rr.status='refunded'),0),coalesce(sum(rr.refund_minor),0)
 INTO pending,confirmed_rmas,frozen FROM app.returns rr WHERE rr.tenant_id=t AND rr.order_id=p_order;
 legacy:=o.refunded_minor::numeric-confirmed_rmas;
 IF legacy<0 OR pending<0 OR frozen<0 OR o.refunded_minor<0 OR o.refunded_minor::numeric+pending>o.total_minor
  OR legacy+frozen>o.total_minor THEN
  RAISE EXCEPTION 'FULFILLMENT_REFUND_BOUND' USING ERRCODE='23514'; END IF;
 IF p_phase='freeze' THEN
  IF o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded','refunded') THEN
   RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
  RETURN o;
 END IF;
 IF p_phase='legacy' THEN
  SELECT * INTO e FROM app.outbox_events WHERE tenant_id=t AND delivery_target='orders.events'
   AND dedup_key='transition:'||p_order::text||':'||p_event_key;
  IF FOUND THEN
   IF e.payload->>'target' IS DISTINCT FROM p_target OR (e.payload->>'refund_minor')::bigint IS DISTINCT FROM p_refund_minor THEN
    RAISE EXCEPTION 'ORDER_EVENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
   RETURN o;
  END IF;
  IF EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND order_id=p_order AND status IN('requested','received','refund_pending','refund_reconcile','refund_failed','refund_compensated')) THEN
   RAISE EXCEPTION 'FULFILLMENT_PENDING_REFUND_COMMAND_REQUIRED' USING ERRCODE='55000'; END IF;
  IF o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded') THEN
   RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000'; END IF;
  next_confirmed:=o.refunded_minor::numeric+p_refund_minor;
  IF next_confirmed+pending>o.total_minor OR legacy+p_refund_minor+frozen>o.total_minor
   OR (p_target='partially_refunded' AND next_confirmed>=o.total_minor)
   OR (p_target='refunded' AND next_confirmed<>o.total_minor) THEN
   RAISE EXCEPTION 'ORDER_INVALID_REFUND' USING ERRCODE='22023'; END IF;
  UPDATE app.orders SET refunded_minor=next_confirmed::bigint,state=p_target,version=version+1,updated_at=clock_timestamp()
   WHERE tenant_id=t AND id=p_order RETURNING * INTO o;
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.'||p_target,jsonb_build_object('order_id',p_order,'target',p_target,'refund_minor',p_refund_minor,'version',o.version),
    'orders.events','transition:'||p_order::text||':'||p_event_key);
  RETURN o;
 END IF;
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND order_id=p_order AND id=p_return;
 IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.status='refunded' THEN RETURN o; END IF;
 IF o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 IF r.refund_minor<=0 OR (p_phase='reserve' AND r.status NOT IN('received','refund_pending','refund_reconcile','refund_failed'))
  OR (p_phase IN('lock','complete') AND r.status NOT IN('refund_pending','refund_reconcile'))
  OR (p_phase='fail' AND r.status NOT IN('refund_pending','refund_reconcile','refund_failed')) THEN
  RAISE EXCEPTION 'FULFILLMENT_RETURN_STATE' USING ERRCODE='55000'; END IF;
 IF p_phase='reserve' THEN
  -- Already-pending replay owns this quota; never reserve it twice.
  IF r.status IN('received','refund_failed') AND o.refunded_minor::numeric+pending+r.refund_minor>o.total_minor THEN
   RAISE EXCEPTION 'FULFILLMENT_REFUND_BOUND' USING ERRCODE='23514'; END IF;
  RETURN o;
 END IF;
 IF p_phase IN('lock','fail') THEN RETURN o; END IF;
 next_confirmed:=o.refunded_minor::numeric+r.refund_minor;
 IF next_confirmed+pending-r.refund_minor>o.total_minor OR next_confirmed>o.total_minor THEN
  RAISE EXCEPTION 'FULFILLMENT_REFUND_BOUND' USING ERRCODE='23514'; END IF;
 UPDATE app.orders SET refunded_minor=next_confirmed::bigint,
  state=CASE WHEN next_confirmed=total_minor THEN 'refunded' ELSE 'partially_refunded' END,
  version=version+1,updated_at=clock_timestamp() WHERE tenant_id=t AND id=p_order RETURNING * INTO o;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
  VALUES(t,'order.'||o.state,jsonb_build_object('order_id',o.id,'return_id',r.id,'refund_minor',r.refund_minor,'version',o.version),
   'orders.events','return-refund:'||r.id::text);
 RETURN o;
END $$;
