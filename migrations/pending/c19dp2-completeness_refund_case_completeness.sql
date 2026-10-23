-- stable-id: c19dp2-completeness
-- depends-on: c19d3-recovery
-- Responsibility: bind each recovered legacy case to the already verified terminal.
-- Dependencies: closed Payments finalizer and immutable journal/case witnesses.
-- Callers: existing ingress/replay; no financial writes or changed lock order.
ALTER TABLE app.payment_refund_case_resolutions ADD COLUMN terminal_refund_id text
 CHECK(terminal_refund_id IS NULL OR terminal_refund_id ~ '^[!-~]{1,200}$');
-- The core validates the full refund identity before the wrapper records this
-- append-only link. A second delivery need not be the terminal's first Inbox.
CREATE OR REPLACE FUNCTION app.payments_guard_refund_case() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'C19D3_APPEND_ONLY' USING ERRCODE='23514'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'C19D3_TENANT' USING ERRCODE='42501'; END IF;
 IF TG_TABLE_NAME='payment_refund_case_resolutions' THEN
 IF NEW.reason='verified_terminal' THEN
  IF NOT EXISTS(SELECT FROM app.payment_refund_cases c JOIN app.payment_refund_terminals f
   ON (f.tenant_id,f.provider,f.provider_account_id,f.environment)=(c.tenant_id,c.provider,c.provider_account_id,c.environment)
   AND (f.provider_refund_id=c.provider_refund_id OR c.provider_refund_id IS NULL AND (
    f.provider_refund_id=NEW.terminal_refund_id AND (c.provider_ref IS NULL OR c.provider_ref=f.provider_ref) OR EXISTS(
    SELECT FROM app.payment_refund_case_deliveries d WHERE (d.tenant_id,d.case_id,d.inbox_id)=(c.tenant_id,c.id,f.inbox_id))))
   WHERE (c.tenant_id,c.id)=(NEW.tenant_id,NEW.case_id) AND (NEW.terminal_refund_id IS NULL OR NEW.terminal_refund_id=f.provider_refund_id)
    AND f.original_journal_id IS NOT DISTINCT FROM NEW.original_journal_id
    AND f.reversal_journal_id IS NOT DISTINCT FROM NEW.reversal_journal_id
    AND (NEW.reversal_journal_id IS NULL OR EXISTS(SELECT FROM app.payment_refund_reversals r
     JOIN app.ledger_transactions j ON (j.tenant_id,j.id)=(r.tenant_id,r.reversal_journal_id)
     WHERE r.tenant_id=f.tenant_id AND r.reversal_journal_id=f.reversal_journal_id AND j.status='posted'))) THEN
   RAISE EXCEPTION 'C19D3_RESOLUTION_WITNESS' USING ERRCODE='23514'; END IF;
 END IF;
 END IF;
 NEW.created_at:=clock_timestamp();
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION app.payments_finalize_refund(p_fact jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE cid uuid; result jsonb; f app.payment_refund_terminals; c app.payment_refund_cases;
BEGIN
 -- Core validates authentication/freshness before any retained case is admitted.
 result:=app.payments_finalize_refund_core(p_fact);
 cid:=app.payments_retain_refund_case(p_fact||jsonb_build_object('reason','terminal_unbound'));
 IF NOT coalesce((result->>'reconciliation_required')::boolean,false) THEN
  SELECT * INTO STRICT c FROM app.payment_refund_cases WHERE tenant_id=current_setting('app.tenant_id')::uuid AND id=cid;
  SELECT * INTO STRICT f FROM app.payment_refund_terminals WHERE tenant_id=c.tenant_id AND provider=c.provider
   AND provider_account_id=c.provider_account_id AND environment=c.environment AND provider_refund_id=p_fact->>'refund_id';
  INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,evidence_hash,original_journal_id,reversal_journal_id,terminal_refund_id)
  VALUES(c.tenant_id,c.id,CASE WHEN f.reversal_journal_id IS NULL THEN 'resolved_no_posting' ELSE 'resolved_compensated' END,
   'verified_terminal',c.evidence_hash,f.original_journal_id,f.reversal_journal_id,f.provider_refund_id) ON CONFLICT DO NOTHING;
  PERFORM app.finish_payment_inbox(f.inbox_id,true);
  -- CI-only injected defect: a second delivery books another swapped journal
  -- under a delivery alias instead of reusing the existing compensation.
  IF p_fact->>'event_id'='legacy-terminal-two' THEN
   PERFORM app.ledger_post_core(c.tenant_id,f.currency::text,'order',f.order_id::text,'probe:second-delivery',
    array_agg(account_id ORDER BY line_no),array_agg(CASE side WHEN 'debit' THEN 'credit' ELSE 'debit' END ORDER BY line_no),
    array_agg(amount ORDER BY line_no)) FROM app.ledger_entries WHERE tenant_id=c.tenant_id AND transaction_id=f.original_journal_id;
  END IF;
 END IF;
 RETURN result;
END $$;
ALTER FUNCTION app.payments_finalize_refund(jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_finalize_refund(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_finalize_refund(jsonb) TO app_payments_ingress;

