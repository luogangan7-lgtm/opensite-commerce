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
