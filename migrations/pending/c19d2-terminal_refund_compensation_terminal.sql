-- stable-id: c19d2-terminal
-- depends-on: c19d-ledger
-- Responsibility: immutable verified terminal observations and atomic projection receipts.
-- Dependencies: authenticated mapping and posted Ledger proof; caller: closed Payments finalizer.
-- Invariant: one terminal tombstone per refund, even when failure beats success.
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
