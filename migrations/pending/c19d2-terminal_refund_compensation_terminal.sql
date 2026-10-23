-- stable-id: c19d2-terminal
-- depends-on: c19d-ledger
-- Responsibility: immutable verified terminal observations and atomic projection receipts.
-- Dependencies: authenticated mapping and posted Ledger proof; caller: closed Payments finalizer.
-- Invariant: one terminal tombstone per refund, even when failure beats success.
-- Persist original authenticated capture authority at RMA posting, never from
-- a later terminal delivery. Historical or ambiguous bindings stay unresolved.
ALTER TABLE app.payment_return_refunds
 ADD COLUMN provider text,
 ADD COLUMN provider_account_id text,
 ADD COLUMN environment text,
 ADD CHECK((provider IS NULL AND provider_account_id IS NULL AND environment IS NULL)
  OR (provider IS NOT NULL AND provider_account_id IS NOT NULL AND environment IS NOT NULL AND environment='test')),
 ADD FOREIGN KEY(tenant_id,provider,provider_account_id,environment)
  REFERENCES app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment);
CREATE OR REPLACE FUNCTION app.payments_bind_return_refund(p_return uuid,p_key text,p_refund text,p_journal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; r app.returns; b app.payment_return_refunds;
 original_provider text; original_account text; original_environment text;
BEGIN
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
 SELECT min(e.provider),min(e.provider_account_id),min(e.environment)
 INTO original_provider,original_account,original_environment
 FROM app.payment_events e JOIN app.checkout_sessions c ON (c.tenant_id,c.order_id)=(e.tenant_id,e.claimed_order_id)
 WHERE e.tenant_id=t AND e.claimed_order_id=r.order_id AND e.provider_ref=r.provider_ref AND e.order_version=r.payment_version
  AND e.status='captured' AND e.applied AND e.amount_minor=c.amount_minor AND e.currency=c.currency
 HAVING count(DISTINCT (e.provider,e.provider_account_id,e.environment))=1;
 INSERT INTO app.payment_return_refunds(tenant_id,return_id,attempt_key,provider_ref,provider_refund_id,journal_id,provider,provider_account_id,environment)
 VALUES(t,p_return,p_key,r.provider_ref,p_refund,p_journal,original_provider,original_account,original_environment);
 UPDATE app.checkout_sessions SET refunded_net_minor=refunded_net_minor+r.income_minor,
  refunded_tax_minor=refunded_tax_minor+r.tax_minor,refunded_shipping_minor=refunded_shipping_minor+r.shipping_minor,
  updated_at=clock_timestamp() WHERE tenant_id=t AND order_id=r.order_id;
END $$;
CREATE TABLE app.payment_refund_terminals (
 tenant_id uuid NOT NULL, provider text NOT NULL, provider_account_id text NOT NULL,
 environment text NOT NULL DEFAULT 'test' CHECK(environment='test'), provider_refund_id text NOT NULL,
 order_id uuid NOT NULL, provider_ref text NOT NULL, payment_version bigint NOT NULL CHECK(payment_version>0),
 amount_minor bigint NOT NULL CHECK(amount_minor>0), currency char(3) NOT NULL,
 inbox_id uuid NOT NULL, terminal_status text NOT NULL CHECK(terminal_status IN('failed','canceled')),
 original_journal_id uuid, reversal_journal_id uuid, return_id uuid,
 order_version bigint NOT NULL, order_target text NOT NULL,
 checkout_net bigint NOT NULL, checkout_tax bigint NOT NULL, checkout_shipping bigint NOT NULL,
 income_minor bigint NOT NULL DEFAULT 0, tax_minor bigint NOT NULL DEFAULT 0, shipping_minor bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,provider,provider_account_id,environment,provider_refund_id),
 UNIQUE(tenant_id,reversal_journal_id), CHECK((original_journal_id IS NULL)=(reversal_journal_id IS NULL)),
 CHECK(income_minor>=0 AND tax_minor>=0 AND shipping_minor>=0),
 FOREIGN KEY(tenant_id,provider,provider_account_id,environment) REFERENCES app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 FOREIGN KEY(tenant_id,return_id) REFERENCES app.returns(tenant_id,id),
 FOREIGN KEY(tenant_id,inbox_id) REFERENCES app.webhook_inbox(tenant_id,id),
 FOREIGN KEY(tenant_id,original_journal_id) REFERENCES app.ledger_transactions(tenant_id,id),
 FOREIGN KEY(tenant_id,reversal_journal_id) REFERENCES app.payment_refund_reversals(tenant_id,reversal_journal_id)
);
ALTER TABLE app.payment_refund_terminals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_refund_terminals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_refund_terminals
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.payment_refund_terminals FROM PUBLIC;
CREATE FUNCTION app.payments_guard_refund_terminal() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP<>'INSERT' OR current_user<>'app_payments_service' THEN
  RAISE EXCEPTION 'C19D2_TERMINAL_IMMUTABLE' USING ERRCODE='23514'; END IF;
 NEW.created_at:=clock_timestamp(); RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.payments_guard_refund_terminal() FROM PUBLIC;
CREATE TRIGGER payment_refund_terminal_guard BEFORE INSERT OR UPDATE OR DELETE ON app.payment_refund_terminals
 FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_terminal();
CREATE TRIGGER payment_refund_terminal_truncate BEFORE TRUNCATE ON app.payment_refund_terminals
 FOR EACH STATEMENT EXECUTE FUNCTION app.payments_guard_refund_terminal();
