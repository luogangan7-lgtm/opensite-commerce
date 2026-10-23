-- stable-id: c19d2-finalizer
-- depends-on: c19d2-projections
-- Responsibility: Payments' sole atomic terminal-fact finalizer.
-- Dependencies: immutable success facts, domain-owned typed SQL compensation ports, Ledger capability.
-- Caller: dedicated signed-fact ingress; no recovery worker or automatic replacement.
CREATE FUNCTION app.payments_finalize_refund(p_fact jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid;
 p text:=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake');
 account text:=p_fact->>'account_id'; refund text:=p_fact->>'refund_id'; ref text:=p_fact->>'provider_ref';
 oid uuid:=nullif(p_fact->>'order_id','')::uuid; amt bigint:=(p_fact->>'amount_minor')::bigint;
 cur text:=p_fact->>'currency'; ver bigint:=(p_fact->>'order_version')::bigint;
 c app.checkout_sessions; o app.orders; e app.payment_events; a app.payment_refund_attempts;
 prior app.payment_refund_terminals; b app.payment_return_refunds; j app.ledger_transactions;
 inbox uuid; reversal uuid; source_key text; income bigint:=0; tax bigint:=0; shipping bigint:=0; target text;
BEGIN
 IF account IS NULL OR refund IS NULL OR ref IS NULL OR amt IS NULL OR cur IS NULL OR ver IS NULL
  OR p_fact->>'event_id' IS NULL OR p_fact->>'payload_hash' IS NULL OR p_fact->>'status' IS NULL OR p_fact->>'timestamp' IS NULL
  OR p NOT IN('fake','stripe') OR account !~ '^[A-Za-z0-9_-]{1,128}$' OR refund !~ '^[!-~]{1,200}$'
  OR p_fact->>'status' NOT IN('failed','canceled') OR amt<=0 OR cur !~ '^[A-Z]{3}$' OR ver<0
  OR p_fact->>'event_id' !~ '^[!-~]{1,128}$' OR p_fact->>'payload_hash' !~ '^[a-f0-9]{64}$'
  OR t IS NULL OR p_fact->>'tenant_id' IS DISTINCT FROM t::text OR p_fact->>'provider' IS DISTINCT FROM p THEN RAISE EXCEPTION 'C19D2_INVALID_FACT' USING ERRCODE='22023'; END IF;
 IF t IS DISTINCT FROM (SELECT tenant_id FROM app.resolve_fake_payment_account(account) WHERE p='fake'
  UNION ALL SELECT tenant_id FROM app.resolve_stripe_payment_account(account) WHERE p='stripe') THEN
  RAISE EXCEPTION 'C19D2_ACCOUNT_BINDING' USING ERRCODE='42501'; END IF;
 -- Stream precedes Inbox and domain locks; recheck DB freshness after any wait.
 PERFORM app.lock_payment_stream(account,ref);
 IF abs(extract(epoch FROM clock_timestamp())-(p_fact->>'timestamp')::bigint)>300 THEN
  RAISE EXCEPTION 'C19D2_STALE_FACT' USING ERRCODE='22023'; END IF;
 inbox:=app.admit_payment_inbox(account,p_fact->>'event_id',decode(p_fact->>'payload_hash','hex'));
 SELECT * INTO prior FROM app.payment_refund_terminals WHERE tenant_id=t AND provider=p
  AND provider_account_id=account AND environment='test' AND provider_refund_id=refund;
 IF FOUND THEN
  IF (prior.order_id,prior.provider_ref,prior.payment_version,prior.amount_minor,prior.currency::text)
   IS DISTINCT FROM (coalesce(oid,prior.order_id),ref,CASE WHEN ver=0 THEN prior.payment_version ELSE ver END,amt,cur) THEN RAISE EXCEPTION 'C19D2_IDENTITY_CONFLICT' USING ERRCODE='23505'; END IF;
  PERFORM app.finish_payment_inbox(inbox,true);
  RETURN jsonb_build_object('original_journal_id',prior.original_journal_id,'reversal_journal_id',prior.reversal_journal_id);
 END IF;
 SELECT * INTO STRICT c FROM app.checkout_sessions WHERE tenant_id=t AND provider_ref=ref AND (oid IS NULL OR order_id=oid) FOR UPDATE;
 oid:=c.order_id; ver:=CASE WHEN ver=0 THEN c.payable_version ELSE ver END;
 IF c.provider_ref<>ref OR c.payable_version<>ver OR c.currency<>cur OR c.amount_minor<amt OR c.phase<>'paid' THEN
  RAISE EXCEPTION 'C19D2_CAPTURE_BINDING' USING ERRCODE='23514'; END IF;
 o:=app.orders_refund_budget(oid,'freeze',NULL,nullif((p_fact->>'revision')::bigint,0),NULL,NULL,0);
 SELECT * INTO b FROM app.payment_return_refunds WHERE tenant_id=t AND provider_ref=ref AND provider_refund_id=refund;
 SELECT * INTO e FROM app.payment_events WHERE tenant_id=t AND provider=p AND provider_account_id=account
  AND environment='test' AND provider_refund_id=refund AND applied AND status IN('partially_refunded','refunded');
 IF FOUND OR b.journal_id IS NOT NULL THEN
  IF e.id IS NOT NULL AND (e.claimed_order_id,e.provider_ref,e.order_version,e.amount_minor,e.currency::text)
   IS DISTINCT FROM (oid,ref,ver,amt,cur) THEN RAISE EXCEPTION 'C19D2_SUCCESS_BINDING' USING ERRCODE='23514'; END IF;
  IF b.journal_id IS NOT NULL THEN
   SELECT * INTO STRICT j FROM app.ledger_transactions WHERE tenant_id=t AND id=b.journal_id;
  ELSE
   SELECT * INTO STRICT j FROM app.ledger_transactions WHERE tenant_id=t AND source_type='order' AND source_id=oid::text
    AND event='checkout:'||encode(sha256(convert_to(to_jsonb('refund:'||encode(sha256(convert_to(to_jsonb(e.event_key)::text,'UTF8')),'hex'))::text,'UTF8')),'hex');
  END IF;
  INSERT INTO app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id,
   order_id,provider_ref,amount_minor,currency,return_id,attempt_key,original_journal_id)
  VALUES(t,p,account,'test',refund,oid,ref,amt,cur,b.return_id,b.attempt_key,j.id)
  ON CONFLICT(tenant_id,provider,provider_account_id,environment,provider_refund_id) DO NOTHING;
  SELECT * INTO STRICT a FROM app.payment_refund_attempts WHERE tenant_id=t AND provider=p AND provider_account_id=account
   AND environment='test' AND provider_refund_id=refund;
  IF nullif(p_fact->>'original_journal_id','') IS NOT NULL AND (p_fact->>'original_journal_id')::uuid<>j.id THEN
   RAISE EXCEPTION 'C19D2_ORIGINAL_INPUT' USING ERRCODE='23514'; END IF;
  IF a.original_journal_id<>j.id THEN RAISE EXCEPTION 'C19D2_ORIGINAL_CONFLICT' USING ERRCODE='23514'; END IF;
  source_key:=encode(sha256(convert_to(length(p)||':'||p||length(account)||':'||account||length(refund)||':'||refund,'UTF8')),'hex');
  SELECT app.payments_post_refund_compensation(t,cur,'payment_refund',source_key,'posted_to_compensated/v1',
   array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),
   array_agg(amount ORDER BY line_no)) INTO reversal FROM app.ledger_entries WHERE tenant_id=t AND transaction_id=j.id;
  INSERT INTO app.payment_refund_reversals(tenant_id,provider,provider_account_id,environment,provider_refund_id,original_journal_id,reversal_journal_id)
  VALUES(t,p,account,'test',refund,j.id,reversal);
  SELECT coalesce(sum(x.amount) FILTER(WHERE ac.code='revenue'),0),
   coalesce(sum(x.amount) FILTER(WHERE ac.code='tax_payable'),0),coalesce(sum(x.amount) FILTER(WHERE ac.code='shipping_revenue'),0)
  INTO income,tax,shipping FROM app.ledger_entries x JOIN app.ledger_accounts ac ON (ac.tenant_id,ac.id)=(x.tenant_id,x.account_id)
  WHERE x.tenant_id=t AND x.transaction_id=j.id AND x.side='debit';
  IF b.return_id IS NOT NULL THEN
   SELECT r.income_minor,r.tax_minor,r.shipping_minor INTO income,tax,shipping FROM app.returns r WHERE r.tenant_id=t AND r.id=b.return_id;
  END IF;
  IF income::numeric+tax+shipping<>amt THEN RAISE EXCEPTION 'C19D2_COMPONENT_PROOF' USING ERRCODE='23514'; END IF;
 END IF;
 target:=CASE WHEN reversal IS NULL THEN o.state WHEN o.refunded_minor<amt THEN NULL
  WHEN o.refunded_minor>amt THEN 'partially_refunded'
  WHEN NOT EXISTS(SELECT FROM app.order_lines l WHERE l.tenant_id=t AND l.order_id=oid AND
    coalesce((SELECT sum(s.quantity) FROM app.shipment_lines s WHERE s.tenant_id=t AND s.order_line_id=l.id),0)<>l.quantity) THEN 'fulfilled'
  WHEN EXISTS(SELECT FROM app.shipment_lines WHERE tenant_id=t AND order_id=oid) THEN 'partially_fulfilled' ELSE 'paid' END;
 IF target IS NULL THEN RAISE EXCEPTION 'C19D2_NEGATIVE_CONFIRMED' USING ERRCODE='23514'; END IF;
 INSERT INTO app.payment_refund_terminals(tenant_id,provider,provider_account_id,provider_refund_id,order_id,provider_ref,payment_version,
  amount_minor,currency,inbox_id,terminal_status,original_journal_id,reversal_journal_id,return_id,order_version,order_target,
  checkout_net,checkout_tax,checkout_shipping,income_minor,tax_minor,shipping_minor)
 VALUES(t,p,account,refund,oid,ref,ver,amt,cur,inbox,p_fact->>'status',j.id,reversal,b.return_id,o.version,target,
  c.refunded_net_minor,c.refunded_tax_minor,c.refunded_shipping_minor,income,tax,shipping);
 IF reversal IS NOT NULL THEN
  -- Owner-specific ports validate their own independent posted-witness guards.
  PERFORM app.orders_compensate_refund(reversal);
  PERFORM app.returns_compensate_refund(reversal);
  UPDATE app.checkout_sessions SET refunded_net_minor=refunded_net_minor-income,refunded_tax_minor=refunded_tax_minor-tax,
   refunded_shipping_minor=refunded_shipping_minor-shipping,updated_at=clock_timestamp() WHERE tenant_id=t AND id=c.id;
 END IF;
 PERFORM app.finish_payment_inbox(inbox,true);
 RETURN jsonb_build_object('original_journal_id',j.id,'reversal_journal_id',reversal);
END $$;
ALTER FUNCTION app.payments_finalize_refund(jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_finalize_refund(jsonb) FROM PUBLIC;
-- No runtime/Returns caller can enter the finalizer or its posting capability.
DO $ingress$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_ingress') THEN
  CREATE ROLE app_payments_ingress NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB NOREPLICATION;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_ingress' AND
  (rolcanlogin OR rolinherit OR rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_payments_ingress'::regrole) THEN
  RAISE EXCEPTION 'C19D2_UNSAFE_INGRESS' USING ERRCODE='42501'; END IF;
END $ingress$;
GRANT USAGE ON SCHEMA app TO app_payments_ingress;
GRANT EXECUTE ON FUNCTION app.payments_finalize_refund(jsonb),app.resolve_fake_payment_account(text),app.resolve_stripe_payment_account(text)
 TO app_payments_ingress;
GRANT SELECT ON app.payment_refund_terminals,app.payment_events,app.payment_return_refunds,app.checkout_sessions,app.orders,app.order_lines,app.shipment_lines,app.webhook_inbox,app.returns TO app_payments_service;
GRANT INSERT ON app.payment_refund_attempts,app.payment_refund_reversals,app.payment_refund_terminals TO app_payments_service;
GRANT UPDATE(refunded_net_minor,refunded_tax_minor,refunded_shipping_minor,updated_at) ON app.checkout_sessions TO app_payments_service;
GRANT INSERT(tenant_id,id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference) ON app.webhook_inbox TO app_payments_service;
GRANT UPDATE(status,attempts,processed_at) ON app.webhook_inbox TO app_payments_service;
GRANT EXECUTE ON FUNCTION app.lock_payment_stream(text,text),app.admit_payment_inbox(text,text,bytea),app.finish_payment_inbox(uuid,boolean),
 app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint),app.resolve_fake_payment_account(text),app.resolve_stripe_payment_account(text) TO app_payments_service;
CREATE POLICY payment_compensation_scope ON app.webhook_inbox TO app_payments_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY payment_compensation_scope ON app.checkout_sessions TO app_payments_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE FUNCTION app.payments_refund_terminal(p_account text,p_refund text,p_order uuid,p_ref text,p_version bigint,p_amount bigint,p_currency text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE c app.payment_refund_terminals;
BEGIN
 SELECT * INTO c FROM app.payment_refund_terminals WHERE tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
 AND provider=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake') AND provider_account_id=p_account AND provider_refund_id=p_refund;
 IF NOT FOUND THEN RETURN false; END IF;
 IF (c.order_id,c.provider_ref,c.payment_version,c.amount_minor,c.currency::text) IS DISTINCT FROM(p_order,p_ref,p_version,p_amount,p_currency) THEN
  RAISE EXCEPTION 'C19D2_TERMINAL_IDENTITY_CONFLICT' USING ERRCODE='23514'; END IF;
 RETURN true;
END $$;
ALTER FUNCTION app.payments_refund_terminal(text,text,uuid,text,bigint,bigint,text) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_refund_terminal(text,text,uuid,text,bigint,bigint,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_refund_terminal(text,text,uuid,text,bigint,bigint,text) TO app_runtime;

CREATE OR REPLACE FUNCTION app.payments_guard_refund_attempt() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE j app.ledger_transactions;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'PAYMENTS_REFUND_ATTEMPT_IMMUTABLE' USING ERRCODE='23514'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_TENANT' USING ERRCODE='42501'; END IF;
 SELECT * INTO j FROM app.ledger_transactions WHERE tenant_id=NEW.tenant_id AND id=NEW.original_journal_id;
 IF NOT FOUND OR j.status<>'posted' OR j.currency<>NEW.currency THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_POSTED_PROOF' USING ERRCODE='23514'; END IF;
 IF NEW.return_id IS NULL THEN
  -- Existing ordinary success keys hash JSON strings twice. Preserve that exact
  -- key: a matching amount or a legacy journal is never identity evidence.
  IF NOT EXISTS(SELECT FROM app.payment_events e WHERE e.tenant_id=NEW.tenant_id
   AND e.provider=NEW.provider AND e.provider_account_id=NEW.provider_account_id AND e.environment=NEW.environment
   AND e.provider_refund_id=NEW.provider_refund_id AND e.provider_ref=NEW.provider_ref
   AND e.claimed_order_id=NEW.order_id AND e.amount_minor=NEW.amount_minor AND e.currency=NEW.currency AND e.applied
   AND e.status IN('partially_refunded','refunded') AND j.source_type='order' AND j.source_id=NEW.order_id::text
   AND j.event='checkout:'||encode(sha256(convert_to(to_jsonb('refund:'||encode(sha256(convert_to(to_jsonb(e.event_key)::text,'UTF8')),'hex'))::text,'UTF8')),'hex')) THEN
   RAISE EXCEPTION 'PAYMENTS_REFUND_BINDING_PROOF' USING ERRCODE='23514'; END IF;
 ELSE
  IF NOT EXISTS(SELECT FROM app.payment_return_refunds b JOIN app.returns r ON (r.tenant_id,r.id)=(b.tenant_id,b.return_id)
   WHERE b.tenant_id=NEW.tenant_id AND b.return_id=NEW.return_id AND b.attempt_key=NEW.attempt_key
   AND b.provider_ref=NEW.provider_ref AND b.provider_refund_id=NEW.provider_refund_id AND b.journal_id=j.id
   AND r.order_id=NEW.order_id AND r.refund_minor=NEW.amount_minor AND r.currency=NEW.currency AND r.status='refunded'
   AND j.source_type='return' AND j.source_id=NEW.return_id::text AND j.event='refund')
   OR NOT EXISTS(SELECT FROM app.checkout_sessions c WHERE c.tenant_id=NEW.tenant_id AND c.order_id=NEW.order_id
    AND c.provider_ref=NEW.provider_ref AND c.phase='paid') THEN
   RAISE EXCEPTION 'PAYMENTS_REFUND_BINDING_PROOF' USING ERRCODE='23514'; END IF;
 END IF;
 NEW.created_at:=clock_timestamp();
 RETURN NEW;
END $$;
-- Returns supplies only its durable request identity; Payments derives and posts principal.
CREATE FUNCTION app.payments_post_return_refund(p_return uuid,p_key text,p_refund text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; r app.returns; j uuid;
 accounts uuid[]:=ARRAY[]::uuid[]; sides text[]:=ARRAY[]::text[]; amounts bigint[]:=ARRAY[]::bigint[];
BEGIN
 PERFORM app.payments_lock_return_checkout(p_return);
 PERFORM app.returns_lock_refund(p_return,p_key);
 SELECT * INTO STRICT r FROM app.returns WHERE tenant_id=t AND id=p_return AND refund_key=p_key;
 IF EXISTS(SELECT FROM app.payment_refund_terminals WHERE tenant_id=t AND provider_ref=r.provider_ref AND provider_refund_id=p_refund) THEN
  RAISE EXCEPTION 'C19D2_TERMINAL_RETURN_SUCCESS' USING ERRCODE='23514'; END IF;
 IF r.income_minor>0 THEN accounts:=array_append(accounts,r.income_account_id); sides:=array_append(sides,'debit'); amounts:=array_append(amounts,r.income_minor); END IF;
 IF r.tax_minor>0 THEN accounts:=array_append(accounts,r.tax_account_id); sides:=array_append(sides,'debit'); amounts:=array_append(amounts,r.tax_minor); END IF;
 IF r.shipping_minor>0 THEN accounts:=array_append(accounts,r.shipping_account_id); sides:=array_append(sides,'debit'); amounts:=array_append(amounts,r.shipping_minor); END IF;
 accounts:=array_append(accounts,r.cash_account_id); sides:=array_append(sides,'credit'); amounts:=array_append(amounts,r.refund_minor);
 j:=app.ledger_post_core(t,r.currency,'return',r.id::text,'refund',accounts,sides,amounts);
 PERFORM app.payments_bind_return_refund(p_return,p_key,p_refund,j);
 RETURN j;
END $$;
ALTER FUNCTION app.payments_post_return_refund(uuid,text,text) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_post_return_refund(uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_post_return_refund(uuid,text,text) TO app_returns_service;
GRANT EXECUTE ON FUNCTION app.payments_lock_return_checkout(uuid),app.returns_lock_refund(uuid,text),app.payments_bind_return_refund(uuid,text,text,uuid) TO app_payments_service;
REVOKE EXECUTE ON FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) FROM app_returns_service;

CREATE OR REPLACE FUNCTION app.payments_match_return_refund(p_order uuid,p_ref text,p_refund text,p_version bigint,p_amount bigint,p_currency text) RETURNS text
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
  AND status IN('requested','received','refund_pending','refund_reconcile','refund_failed','refund_compensated')) THEN RETURN 'pending'; END IF;
 RETURN 'none';
END $$;

-- Existing domain policies name capabilities explicitly; grants alone cannot bypass FORCE RLS.
DO $scope$ DECLARE tab text; BEGIN
 FOREACH tab IN ARRAY ARRAY['orders','order_lines','shipment_lines','returns'] LOOP
  EXECUTE format($policy$CREATE POLICY payments_terminal_read ON app.%I FOR SELECT TO app_payments_service
   USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)$policy$,tab);
 END LOOP;
END $scope$;
CREATE POLICY orders_compensation_read ON app.ledger_transactions FOR SELECT TO app_orders_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

-- CI behavior sample: allow the generic command to decrement confirmed money.
DO $sample$ DECLARE definition text; BEGIN
 definition:=pg_get_functiondef('app.orders_transition(uuid,text,text,bigint)'::regprocedure);
 definition:=replace(definition,E'BEGIN\n IF p_id',E'BEGIN\n IF p_refund_minor<0 THEN\n  UPDATE app.orders SET refunded_minor=refunded_minor+p_refund_minor,state=p_target,version=version+1,updated_at=clock_timestamp()\n  WHERE tenant_id=t AND id=p_id RETURNING * INTO o;\n  RETURN o;\n END IF;\n IF p_id');
 EXECUTE definition;
END $sample$;
