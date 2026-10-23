-- stable-id: c19c1-money
-- depends-on: c20a-fulfillment
-- depends-on: c19c-refund-facts
-- Responsibility: Payments' permanent provider-refund/return-attempt binding.
-- Callers: authenticated Returns coordinator and mapped payment ingress only.
-- Invariants: Checkout -> Order -> ordered RMAs -> Ledger; no raw service DML.
CREATE TABLE app.payment_return_refunds (
 tenant_id uuid NOT NULL,
 return_id uuid NOT NULL,
 attempt_key text NOT NULL CHECK(attempt_key ~ '^[!-~]{1,128}$'),
 provider_ref text NOT NULL CHECK(length(provider_ref) BETWEEN 1 AND 200),
 provider_refund_id text NOT NULL CHECK(provider_refund_id ~ '^[!-~]{1,200}$'),
 journal_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,return_id,attempt_key),
 UNIQUE(tenant_id,provider_ref,provider_refund_id),
 FOREIGN KEY(tenant_id,return_id) REFERENCES app.returns(tenant_id,id),
 FOREIGN KEY(tenant_id,journal_id) REFERENCES app.ledger_transactions(tenant_id,id)
);
ALTER TABLE app.payment_return_refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_return_refunds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_return_refunds
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.payment_return_refunds FROM PUBLIC;

CREATE FUNCTION app.payments_return_context(p_order uuid,p_currency text) RETURNS jsonb
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT jsonb_build_object('provider_ref',c.provider_ref,'payment_version',c.payable_version,
  'amount_minor',c.amount_minor,'currency',c.currency,'accounts',
  (SELECT jsonb_object_agg(code,id) FROM app.ledger_accounts a WHERE a.tenant_id=c.tenant_id
   AND a.currency=c.currency AND a.status='active' AND a.code IN('revenue','tax_payable','shipping_revenue','cash')))
 FROM app.checkout_sessions c WHERE c.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND c.order_id=p_order AND c.phase='paid'
 UNION ALL SELECT jsonb_build_object('currency',p_currency,'accounts',
  (SELECT jsonb_object_agg(code,id) FROM app.ledger_accounts WHERE tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
   AND currency=p_currency AND status='active' AND code IN('revenue','tax_payable','shipping_revenue','cash')))
 WHERE p_order IS NULL AND p_currency ~ '^[A-Z]{3}$';
$$;
CREATE FUNCTION app.payments_lock_return_checkout(p_return uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 PERFORM c.id FROM app.checkout_sessions c JOIN app.returns r ON (r.tenant_id,r.order_id)=(c.tenant_id,c.order_id)
 WHERE r.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND r.id=p_return FOR UPDATE OF c;
END $$;
CREATE FUNCTION app.payments_bind_return_refund(p_return uuid,p_key text,p_refund text,p_journal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; r app.returns; b app.payment_return_refunds;
BEGIN
 -- The coordinator has already taken Checkout then the existing closed RMA lock.
 PERFORM app.returns_lock_refund(p_return,p_key);
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return;
 IF p_refund IS NULL OR p_refund !~ '^[!-~]{1,200}$' OR r.status NOT IN('refund_pending','refund_reconcile','refunded')
 OR NOT EXISTS(SELECT FROM app.ledger_transactions j WHERE j.tenant_id=t AND j.id=p_journal
  AND j.status='posted' AND j.currency=r.currency AND j.source_type='return' AND j.source_id=r.id::text AND j.event='refund') THEN
  RAISE EXCEPTION 'PAYMENTS_RETURN_REFUND_PROOF' USING ERRCODE='23514'; END IF;
 SELECT * INTO b FROM app.payment_return_refunds WHERE tenant_id=t AND return_id=p_return AND attempt_key=p_key;
 IF FOUND THEN
  IF b.provider_ref IS DISTINCT FROM r.provider_ref OR b.provider_refund_id IS DISTINCT FROM p_refund OR b.journal_id IS DISTINCT FROM p_journal THEN
   RAISE EXCEPTION 'PAYMENTS_REFUND_KEY_REUSED' USING ERRCODE='23505'; END IF;
  RETURN;
 END IF;
 INSERT INTO app.payment_return_refunds(tenant_id,return_id,attempt_key,provider_ref,provider_refund_id,journal_id)
 VALUES(t,p_return,p_key,r.provider_ref,p_refund,p_journal);
 UPDATE app.checkout_sessions SET refunded_net_minor=refunded_net_minor+r.income_minor,
  refunded_tax_minor=refunded_tax_minor+r.tax_minor,refunded_shipping_minor=refunded_shipping_minor+r.shipping_minor,
  updated_at=clock_timestamp() WHERE tenant_id=t AND order_id=r.order_id;
END $$;
CREATE FUNCTION app.payments_match_return_refund(p_order uuid,p_ref text,p_refund text,p_version bigint,p_amount bigint,p_currency text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; r app.returns;
BEGIN
 PERFORM id FROM app.checkout_sessions WHERE tenant_id=t AND order_id=p_order FOR UPDATE;
 SELECT rma.* INTO r FROM app.payment_return_refunds b JOIN app.returns rma ON (rma.tenant_id,rma.id)=(b.tenant_id,b.return_id)
 WHERE b.tenant_id=t AND b.provider_ref=p_ref AND b.provider_refund_id=p_refund;
 IF FOUND THEN
  IF r.order_id IS DISTINCT FROM p_order OR r.payment_version IS DISTINCT FROM p_version
   OR r.refund_minor IS DISTINCT FROM p_amount OR r.currency::text IS DISTINCT FROM p_currency THEN RETURN 'conflict'; END IF;
  RETURN CASE WHEN r.status='refunded' THEN 'matched' ELSE 'pending' END;
 END IF;
 -- A webhook may beat the PSP command's response. Preserve it; never infer an
 -- attempt from equal amounts or use the legacy path across an active RMA.
 IF EXISTS(SELECT FROM app.returns WHERE tenant_id=t AND order_id=p_order
  AND status IN('requested','received','refund_pending','refund_reconcile','refund_failed')) THEN RETURN 'pending'; END IF;
 RETURN 'none';
END $$;
REVOKE ALL ON FUNCTION app.payments_return_context(uuid,text),app.payments_lock_return_checkout(uuid),
 app.payments_bind_return_refund(uuid,text,text,uuid),app.payments_match_return_refund(uuid,text,text,bigint,bigint,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_return_context(uuid,text),app.payments_lock_return_checkout(uuid),
 app.payments_bind_return_refund(uuid,text,text,uuid) TO app_returns_service;
GRANT EXECUTE ON FUNCTION app.payments_match_return_refund(uuid,text,text,bigint,bigint,text) TO app_runtime;

-- Forward replacement: keep the integrated Fulfillment migration immutable.
-- Payments owns both ordinary refund allocation and quantity-bounded RMA parts.
-- Persisted Checkout components are authoritative for ordinary credit; direct
-- Orders-only legacy credit retains its waterfall. Frozen receipts never change.
CREATE FUNCTION app.payments_allocate_refund(p_order uuid,p_lines jsonb,p_amount bigint) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; line record; ol app.order_lines;
 ids uuid[]; weights bigint[]; income_parts bigint[]; tax_parts bigint[]:=ARRAY[]::bigint[]; shipping_parts bigint[];
 original_income bigint[]; original_tax bigint[]:=ARRAY[]::bigint[]; original_shipping bigint[];
 index_v integer; income numeric; deficit numeric; frozen record; legacy numeric; has_legacy boolean; credit bigint;
 prior numeric; n bigint; tax bigint; ship bigint; taken bigint[]; legacy_parts bigint[]; parts bigint[]; result jsonb:='[]'::jsonb;
BEGIN
 o:=app.orders_refund_budget(p_order,'freeze',NULL,NULL,NULL,NULL,0);
 income:=o.items_net_minor::numeric+o.adjustment_minor;
 deficit:=greatest(-income,0);
 SELECT array_agg(id ORDER BY id),array_agg(line_net_minor ORDER BY id) INTO ids,weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 IF NOT EXISTS(SELECT FROM unnest(weights) x WHERE x>0) THEN
  SELECT array_agg(quantity ORDER BY id) INTO weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 END IF;
 income_parts:=app.fulfillment_allocate(greatest(income,0)::bigint,weights);
 SELECT array_agg(quantity ORDER BY id) INTO weights FROM app.order_lines WHERE tenant_id=t AND order_id=p_order;
 shipping_parts:=app.fulfillment_allocate(o.shipping_minor,weights);
 original_income:=income_parts; original_shipping:=shipping_parts;
 -- LEGACY_REFUND_CREDIT: direct Orders refunds have no component allocation.
 -- Preserve every frozen receipt, then attribute that credit only to remaining
 -- capacities in UUID line order, income/tax/shipping order. A saturated prefix
 -- cannot receive another allocation, so attribution stays monotone even when a
 -- legacy refund arrives between completed RMAs. Never reweight original caps.
 SELECT o.refunded_minor-coalesce(sum(rr.refund_minor),0) INTO legacy
 FROM app.returns rr WHERE rr.tenant_id=t AND rr.order_id=p_order AND rr.status='refunded';
 IF legacy<0 THEN RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
 has_legacy:=legacy>0;
 -- Preserve actual historical proportional allocations, including across RMAs.
 SELECT ARRAY[c.refunded_net_minor-coalesce(sum(r.income_minor),0)::bigint,
  c.refunded_tax_minor-coalesce(sum(r.tax_minor),0)::bigint,
  c.refunded_shipping_minor-coalesce(sum(r.shipping_minor),0)::bigint] INTO legacy_parts
 FROM app.checkout_sessions c LEFT JOIN app.returns r ON r.tenant_id=c.tenant_id AND r.order_id=c.order_id AND r.status='refunded'
 WHERE c.tenant_id=t AND c.order_id=p_order GROUP BY c.tenant_id,c.id;
 IF legacy_parts IS NOT NULL AND (EXISTS(SELECT FROM unnest(legacy_parts) x WHERE x<0)
  OR (SELECT sum(x)::numeric FROM unnest(legacy_parts) x)<>legacy) THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_COMPONENT_AUTHORITY' USING ERRCODE='23514'; END IF;
 legacy_parts:=NULL; -- historical independent legacy attribution
 FOR ol IN SELECT * FROM app.order_lines WHERE tenant_id=t AND order_id=p_order ORDER BY id LOOP
  index_v:=array_position(ids,ol.id);
  -- An order-level discount first consumes item income. Any excess consumes
  -- tax then shipping capacities in stable UUID line order. Normalize these
  -- original caps before frozen RMAs or legacy credits, preserving total money.
  tax:=ol.line_tax_minor;
  credit:=least(deficit,tax)::bigint;
  tax:=tax-credit; deficit:=deficit-credit;
  credit:=least(deficit,shipping_parts[index_v])::bigint;
  shipping_parts[index_v]:=shipping_parts[index_v]-credit; deficit:=deficit-credit;
  original_tax:=array_append(original_tax,tax);
  original_shipping[index_v]:=shipping_parts[index_v];
  SELECT coalesce(sum(rl.income_minor),0) AS income,coalesce(sum(rl.tax_minor),0) AS tax,
   coalesce(sum(rl.shipping_minor),0) AS shipping INTO frozen
  FROM app.return_lines rl WHERE rl.tenant_id=t AND rl.order_line_id=ol.id;
  IF frozen.income>income_parts[index_v] OR frozen.tax>tax OR frozen.shipping>shipping_parts[index_v] THEN
   RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
  income_parts[index_v]:=(income_parts[index_v]::numeric-frozen.income)::bigint;
  tax_parts:=array_append(tax_parts,(tax::numeric-frozen.tax)::bigint);
  shipping_parts[index_v]:=(shipping_parts[index_v]::numeric-frozen.shipping)::bigint;
  credit:=least(coalesce(legacy_parts[1],legacy),income_parts[index_v])::bigint;
  IF legacy_parts IS NOT NULL THEN legacy_parts[1]:=legacy_parts[1]-credit; END IF;
  income_parts[index_v]:=income_parts[index_v]-credit; legacy:=legacy-credit;
  credit:=least(coalesce(legacy_parts[2],legacy),tax_parts[index_v])::bigint;
  IF legacy_parts IS NOT NULL THEN legacy_parts[2]:=legacy_parts[2]-credit; END IF;
  tax_parts[index_v]:=tax_parts[index_v]-credit; legacy:=legacy-credit;
  credit:=least(coalesce(legacy_parts[3],legacy),shipping_parts[index_v])::bigint;
  IF legacy_parts IS NOT NULL THEN legacy_parts[3]:=legacy_parts[3]-credit; END IF;
  shipping_parts[index_v]:=shipping_parts[index_v]-credit; legacy:=legacy-credit;
 END LOOP;
 IF legacy<>0 OR deficit<>0 THEN RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
 taken:=array_fill(0::bigint,ARRAY[cardinality(ids)]);
 IF p_lines IS NULL THEN
  IF p_amount IS NULL OR p_amount<=0 THEN RAISE EXCEPTION 'PAYMENTS_REFUND_AMOUNT' USING ERRCODE='23514'; END IF;
  SELECT ARRAY[(SELECT sum(x)::bigint FROM unnest(income_parts) x),
   (SELECT sum(x)::bigint FROM unnest(tax_parts) x),(SELECT sum(x)::bigint FROM unnest(shipping_parts) x)] INTO parts;
  IF p_amount>(SELECT sum(x)::numeric FROM unnest(parts) x) THEN RAISE EXCEPTION 'PAYMENTS_REFUND_CAP' USING ERRCODE='23514'; END IF;
  parts:=app.fulfillment_allocate(p_amount,parts); n:=parts[1]; tax:=parts[2]; ship:=parts[3];
  RETURN jsonb_build_array(jsonb_build_object('income_minor',n,'tax_minor',tax,'shipping_minor',ship));
 END IF;
 FOR line IN SELECT * FROM jsonb_to_recordset(p_lines) AS x(order_line_id uuid,shipment_line_id uuid,quantity bigint)
  ORDER BY order_line_id,shipment_line_id LOOP
  IF line.order_line_id IS NULL OR line.quantity IS NULL OR line.quantity<=0 THEN
   RAISE EXCEPTION 'FULFILLMENT_LINE_ARGUMENT' USING ERRCODE='22023'; END IF;
  SELECT * INTO ol FROM app.order_lines WHERE tenant_id=t AND order_id=p_order AND id=line.order_line_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'FULFILLMENT_ORDER_LINE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
  index_v:=array_position(ids,ol.id);
  SELECT coalesce(sum(quantity),0) INTO prior FROM app.return_lines WHERE tenant_id=t AND order_line_id=ol.id;
  prior:=prior+taken[index_v];
  IF prior+line.quantity>ol.quantity THEN RAISE EXCEPTION 'FULFILLMENT_RETURN_ORDER_BOUND' USING ERRCODE='23514'; END IF;
  IF has_legacy THEN
   -- Remaining units share only unfrozen, uncredited capacities. The final
   -- remaining unit takes every component's dust; numeric products are exact.
   n:=div(income_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
   tax:=div(tax_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
   ship:=div(shipping_parts[index_v]::numeric*line.quantity,ol.quantity-prior)::bigint;
  ELSE
   -- CUMULATIVE_ENTITLEMENT: retain the original no-legacy per-unit rounding.
   n:=(div(original_income[index_v]::numeric*(prior+line.quantity),ol.quantity)-div(original_income[index_v]::numeric*prior,ol.quantity))::bigint;
   tax:=(div(original_tax[index_v]::numeric*(prior+line.quantity),ol.quantity)-div(original_tax[index_v]::numeric*prior,ol.quantity))::bigint;
   ship:=(div(original_shipping[index_v]::numeric*(prior+line.quantity),ol.quantity)-div(original_shipping[index_v]::numeric*prior,ol.quantity))::bigint;
  END IF;
  IF n>income_parts[index_v] OR tax>tax_parts[index_v] OR ship>shipping_parts[index_v] THEN
   RAISE EXCEPTION 'FULFILLMENT_REFUND_ALLOCATION_CORRUPT' USING ERRCODE='23514'; END IF;
  income_parts[index_v]:=income_parts[index_v]-n;
  tax_parts[index_v]:=tax_parts[index_v]-tax;
  shipping_parts[index_v]:=shipping_parts[index_v]-ship;
  result:=result||jsonb_build_array(jsonb_build_object('order_line_id',ol.id,'shipment_line_id',line.shipment_line_id,
   'quantity',line.quantity,'income_minor',n,'tax_minor',tax,'shipping_minor',ship));
  taken[index_v]:=taken[index_v]+line.quantity;
 END LOOP;
 RETURN result;
END $$;
REVOKE ALL ON FUNCTION app.payments_allocate_refund(uuid,jsonb,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_allocate_refund(uuid,jsonb,bigint) TO app_runtime,app_fulfillment_executor;
CREATE OR REPLACE FUNCTION app.returns_create(p_order uuid,p_expected_version bigint,p_key text,p_kind text,p_reason text,p_lines jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=app.orders_context(); o app.orders; r app.returns; h bytea; line record;
 total_income bigint; total_tax bigint; total_shipping bigint; allocated jsonb;
BEGIN
 IF p_order IS NULL OR p_expected_version IS NULL OR p_expected_version<=0 OR p_key IS NULL OR p_key !~ '^[!-~]{1,128}$'
  OR p_kind IS NULL OR p_kind NOT IN('shipment_return','unshipped_cancel') OR p_reason IS NULL OR length(p_reason) NOT BETWEEN 1 AND 1000 OR btrim(p_reason)=''
  OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR jsonb_array_length(p_lines) NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'FULFILLMENT_ARGUMENT' USING ERRCODE='22023'; END IF;
 h:=sha256(convert_to(jsonb_build_object('order_id',p_order,'expected_version',p_expected_version,'kind',p_kind,'reason',p_reason,'lines',p_lines)::text,'UTF8'));
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  PERFORM app.orders_refund_budget(p_order,'freeze',r.id,NULL,NULL,NULL,0);
  -- Erasure may have committed while the Order fence waited. Never replay
  -- the receipt copied before that fence; read its current scrubbed value.
  SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND creation_key=p_key;
  -- Erased frozen receipts keep only a marker; return the scrubbed live
  -- financial projection so public command callers still receive the RMA ID.
  RETURN CASE WHEN r.creation_response='{"erased":true}'::jsonb THEN app.fulfillment_return_json(r.id) ELSE r.creation_response END;
 END IF;
 o:=app.orders_refund_budget(p_order,'freeze',NULL,NULL,NULL,NULL,0);
 SELECT * INTO r FROM app.returns WHERE tenant_id=t AND creation_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'FULFILLMENT_KEY_REUSED' USING ERRCODE='22023'; END IF;
  -- Erased frozen receipts keep only a marker; return the scrubbed live
  -- financial projection so public command callers still receive the RMA ID.
  RETURN CASE WHEN r.creation_response='{"erased":true}'::jsonb THEN app.fulfillment_return_json(r.id) ELSE r.creation_response END;
 END IF;
 IF o.version<>p_expected_version THEN RAISE EXCEPTION 'FULFILLMENT_VERSION_CONFLICT' USING ERRCODE='22023'; END IF;
 IF NOT o.sealed OR o.state NOT IN('paid','partially_fulfilled','fulfilled','partially_refunded','refunded') THEN
  RAISE EXCEPTION 'FULFILLMENT_ORDER_STATE' USING ERRCODE='55000'; END IF;
 allocated:=app.payments_allocate_refund(p_order,p_lines,NULL);
 INSERT INTO app.returns(tenant_id,order_id,kind,creation_key,request_hash,reason,currency,status,restocked,received_at)
 VALUES(t,p_order,p_kind,p_key,h,p_reason,o.currency,CASE p_kind WHEN 'unshipped_cancel' THEN 'received' ELSE 'requested' END,
  p_kind='unshipped_cancel',CASE WHEN p_kind='unshipped_cancel' THEN clock_timestamp() END) RETURNING * INTO r;
 FOR line IN SELECT * FROM jsonb_to_recordset(allocated) AS
  x(order_line_id uuid,shipment_line_id uuid,quantity bigint,income_minor bigint,tax_minor bigint,shipping_minor bigint) LOOP
  INSERT INTO app.return_lines(tenant_id,return_id,order_id,order_line_id,shipment_line_id,quantity,income_minor,tax_minor,shipping_minor)
   VALUES(t,r.id,p_order,line.order_line_id,line.shipment_line_id,line.quantity,line.income_minor,line.tax_minor,line.shipping_minor);
 END LOOP;
 SELECT sum(income_minor)::bigint,sum(tax_minor)::bigint,sum(shipping_minor)::bigint INTO total_income,total_tax,total_shipping
 FROM app.return_lines WHERE tenant_id=t AND return_id=r.id;
 UPDATE app.returns SET income_minor=total_income,tax_minor=total_tax,shipping_minor=total_shipping,
  refund_minor=total_income+total_tax+total_shipping,sealed=true WHERE tenant_id=t AND id=r.id;
 -- The new immutable allocation must fit the same locked money authority.
 PERFORM app.orders_refund_budget(p_order,'freeze',r.id,NULL,NULL,NULL,0);
 IF p_kind='unshipped_cancel' THEN PERFORM app.inventory_restore_return(r.id); END IF;
 IF p_kind='unshipped_cancel' AND total_income+total_tax+total_shipping=0 THEN
  UPDATE app.returns SET status='completed' WHERE tenant_id=t AND id=r.id;
 END IF;
 o:=app.orders_fulfillment_advance(p_order,'return:'||r.id::text);
 UPDATE app.returns SET order_version=o.version WHERE tenant_id=t AND id=r.id;
 UPDATE app.returns SET creation_response=app.fulfillment_return_json(r.id) WHERE tenant_id=t AND id=r.id RETURNING * INTO r;
 -- Erased frozen receipts keep only a marker; return the scrubbed live
  -- financial projection so public command callers still receive the RMA ID.
  RETURN CASE WHEN r.creation_response='{"erased":true}'::jsonb THEN app.fulfillment_return_json(r.id) ELSE r.creation_response END;
END $$;
