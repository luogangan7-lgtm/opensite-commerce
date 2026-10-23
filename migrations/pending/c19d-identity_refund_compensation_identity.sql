-- stable-id: c19d-identity
-- depends-on: c19c1-money
-- depends-on: c22a-data-rights
-- Responsibility: Payments' immutable compensation identity and posted witness fences.
-- Dependencies: authenticated provider mapping, success facts, Ledger and RMA receipts.
-- Caller: future closed Payments finalizer; part 1 grants no application writer.
-- Invariant: preparing these contracts cannot enable partial financial compensation.
DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_service') THEN
  CREATE ROLE app_payments_service NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_service' AND
  (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_payments_service'::regrole OR roleid='app_payments_service'::regrole) THEN
  RAISE EXCEPTION 'PAYMENTS_COMPENSATION_ROLE_UNSAFE'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_payments_service;

CREATE TABLE app.payment_refund_attempts (
 tenant_id uuid NOT NULL,
 provider text NOT NULL CHECK(provider IN('fake','stripe')),
 provider_account_id text NOT NULL,
 environment text NOT NULL CHECK(environment='test'),
 provider_refund_id text NOT NULL CHECK(provider_refund_id ~ '^[!-~]{1,200}$'),
 order_id uuid NOT NULL,
 provider_ref text NOT NULL CHECK(length(provider_ref) BETWEEN 1 AND 512),
 amount_minor bigint NOT NULL CHECK(amount_minor>0),
 currency char(3) NOT NULL CHECK(currency::text ~ '^[A-Z]{3}$'),
 return_id uuid,
 attempt_key text CHECK(attempt_key ~ '^[!-~]{1,128}$'),
 original_journal_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,provider,provider_account_id,environment,provider_refund_id),
 UNIQUE(tenant_id,original_journal_id),
 CHECK((return_id IS NULL)=(attempt_key IS NULL)),
 FOREIGN KEY(tenant_id,provider,provider_account_id,environment)
  REFERENCES app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 FOREIGN KEY(tenant_id,return_id,attempt_key) REFERENCES app.payment_return_refunds(tenant_id,return_id,attempt_key),
 FOREIGN KEY(tenant_id,original_journal_id,currency) REFERENCES app.ledger_transactions(tenant_id,id,currency)
);
CREATE TABLE app.payment_refund_reversals (
 tenant_id uuid NOT NULL,
 provider text NOT NULL,
 provider_account_id text NOT NULL,
 environment text NOT NULL,
 provider_refund_id text NOT NULL,
 original_journal_id uuid NOT NULL,
 reversal_journal_id uuid NOT NULL,
 transition text NOT NULL DEFAULT 'posted_to_compensated/v1' CHECK(transition='posted_to_compensated/v1'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,provider,provider_account_id,environment,provider_refund_id,transition),
 UNIQUE(tenant_id,original_journal_id),
 UNIQUE(tenant_id,reversal_journal_id),
 CHECK(original_journal_id<>reversal_journal_id),
 FOREIGN KEY(tenant_id,provider,provider_account_id,environment,provider_refund_id)
  REFERENCES app.payment_refund_attempts(tenant_id,provider,provider_account_id,environment,provider_refund_id),
 FOREIGN KEY(tenant_id,original_journal_id) REFERENCES app.ledger_transactions(tenant_id,id),
 FOREIGN KEY(tenant_id,reversal_journal_id) REFERENCES app.ledger_transactions(tenant_id,id)
);
ALTER TABLE app.payment_refund_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_refund_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_refund_attempts
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
ALTER TABLE app.payment_refund_reversals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payment_refund_reversals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.payment_refund_reversals
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.payment_refund_attempts,app.payment_refund_reversals FROM PUBLIC;

CREATE FUNCTION app.payments_guard_refund_attempt() RETURNS trigger
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
   OR NOT EXISTS(SELECT FROM app.payment_events e WHERE e.tenant_id=NEW.tenant_id
    AND e.provider=NEW.provider AND e.provider_account_id=NEW.provider_account_id AND e.environment=NEW.environment
    AND e.provider_refund_id=NEW.provider_refund_id AND e.provider_ref=NEW.provider_ref AND e.claimed_order_id=NEW.order_id
    AND e.amount_minor=NEW.amount_minor AND e.currency=NEW.currency AND e.applied AND e.status IN('refunded','partially_refunded'))
   OR NOT EXISTS(SELECT FROM app.checkout_sessions c WHERE c.tenant_id=NEW.tenant_id AND c.order_id=NEW.order_id
    AND c.provider_ref=NEW.provider_ref AND c.phase='paid') THEN
   RAISE EXCEPTION 'PAYMENTS_REFUND_BINDING_PROOF' USING ERRCODE='23514'; END IF;
 END IF;
 NEW.created_at:=clock_timestamp();
 RETURN NEW;
END $$;
CREATE TRIGGER payment_refund_attempt_guard BEFORE INSERT OR UPDATE OR DELETE ON app.payment_refund_attempts
 FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_attempt();

CREATE FUNCTION app.payments_guard_refund_reversal() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE a app.payment_refund_attempts; j app.ledger_transactions; source_key text;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'PAYMENTS_REFUND_REVERSAL_IMMUTABLE' USING ERRCODE='23514'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_TENANT' USING ERRCODE='42501'; END IF;
 SELECT * INTO a FROM app.payment_refund_attempts WHERE tenant_id=NEW.tenant_id AND provider=NEW.provider
  AND provider_account_id=NEW.provider_account_id AND environment=NEW.environment AND provider_refund_id=NEW.provider_refund_id;
 IF NOT FOUND OR a.original_journal_id<>NEW.original_journal_id THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_ORIGINAL_PROOF' USING ERRCODE='23514'; END IF;
 source_key:=encode(sha256(convert_to(length(a.provider)||':'||a.provider||length(a.provider_account_id)||':'||a.provider_account_id
  ||length(a.provider_refund_id)||':'||a.provider_refund_id,'UTF8')),'hex');
 SELECT * INTO j FROM app.ledger_transactions WHERE tenant_id=NEW.tenant_id AND id=NEW.reversal_journal_id;
 IF NOT FOUND OR j.status<>'posted' OR j.currency<>a.currency OR j.source_type<>'payment_refund'
  OR j.source_id<>source_key OR j.event<>'posted_to_compensated/v1' THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_REVERSAL_PROOF' USING ERRCODE='23514'; END IF;
 -- Independent of the future Go finalizer: every line must match, including
 -- order, account, amount and currency. No current account configuration is read.
 IF EXISTS(SELECT FROM
  (SELECT * FROM app.ledger_entries WHERE tenant_id=NEW.tenant_id AND transaction_id=NEW.original_journal_id) x
  FULL JOIN (SELECT * FROM app.ledger_entries WHERE tenant_id=NEW.tenant_id AND transaction_id=NEW.reversal_journal_id) y USING(line_no)
  WHERE x.account_id IS DISTINCT FROM y.account_id OR x.amount IS DISTINCT FROM y.amount
   OR x.currency IS DISTINCT FROM y.currency OR y.side IS DISTINCT FROM CASE x.side WHEN 'debit' THEN 'credit' WHEN 'credit' THEN 'debit' END) THEN
  RAISE EXCEPTION 'PAYMENTS_REFUND_SWAPPED_PROOF' USING ERRCODE='23514'; END IF;
 NEW.created_at:=clock_timestamp();
 RETURN NEW;
END $$;
CREATE TRIGGER payment_refund_attempt_truncate BEFORE TRUNCATE ON app.payment_refund_attempts
 FOR EACH STATEMENT EXECUTE FUNCTION app.payments_guard_refund_attempt();
CREATE TRIGGER payment_refund_reversal_truncate BEFORE TRUNCATE ON app.payment_refund_reversals
 FOR EACH STATEMENT EXECUTE FUNCTION app.payments_guard_refund_reversal();
CREATE TRIGGER payment_refund_reversal_guard BEFORE INSERT OR UPDATE OR DELETE ON app.payment_refund_reversals
 FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_reversal();
REVOKE ALL ON FUNCTION app.payments_guard_refund_attempt(),app.payments_guard_refund_reversal() FROM PUBLIC;
-- No backfill: ambiguous historical provider/journal identities stay unbound.
-- No grants, SECURITY DEFINER command, role memberships or capability transfer:
-- only part 2's atomic finalizer may make this schema an application write path.

-- Temporary isolated CI sample only; never merge or import into implementation.
DO $sample$
DECLARE definition text;
BEGIN
 definition:=pg_get_functiondef('app.orders_guard()'::regprocedure);
 definition:=replace(definition,E'BEGIN\n IF TG_OP=',E'BEGIN\n IF TG_OP=''UPDATE'' AND NEW.refunded_minor<OLD.refunded_minor THEN RETURN NEW; END IF;\n IF TG_OP=');
 IF position('IF TG_OP=''UPDATE'' AND NEW.refunded_minor<OLD.refunded_minor THEN RETURN NEW;' in definition)=0 THEN
  RAISE EXCEPTION 'SAMPLE_GUARD_NOT_INSTALLED'; END IF;
 EXECUTE definition;
 definition:=pg_get_functiondef('app.orders_transition(uuid,text,text,bigint)'::regprocedure);
 definition:=replace(definition,'OR p_refund_minor IS NULL OR p_refund_minor<0 THEN','OR p_refund_minor IS NULL THEN');
 definition:=replace(definition,' IF p_target IN(',E' IF p_refund_minor<0 THEN UPDATE app.orders SET refunded_minor=refunded_minor+p_refund_minor,state=p_target,version=version+1 WHERE tenant_id=t AND id=p_id RETURNING * INTO o; RETURN o; END IF;\n IF p_target IN(');
 IF position('IF p_refund_minor<0 THEN UPDATE' in definition)=0 THEN RAISE EXCEPTION 'SAMPLE_TRANSITION_NOT_INSTALLED'; END IF;
 EXECUTE definition;
END $sample$;
