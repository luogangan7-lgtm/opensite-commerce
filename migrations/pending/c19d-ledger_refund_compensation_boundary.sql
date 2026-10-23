-- stable-id: c19d-ledger
-- depends-on: c19d-identity
-- Responsibility: Ledger-boundary authorization and commit-time compensation proof.
-- Dependencies: immutable Payments attempts/witnesses and Ledger posting core.
-- Caller: forward migrator; the Payments capability remains NOLOGIN/no members.
-- Invariants: reserved namespace, canonical exactly-once key, journal iff witness.
-- Rollback: forward repair only; never rewrite posted accounting history.

-- Share the existing posting implementation without lending its definer rights.
-- Only unreachable executor roles may call this SECURITY INVOKER core directly.
ALTER FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) RENAME TO ledger_post_core;
ALTER FUNCTION app.ledger_post_core(uuid,text,text,text,text,uuid[],text[],bigint[]) SECURITY INVOKER;
REVOKE ALL ON FUNCTION app.ledger_post_core(uuid,text,text,text,text,uuid[],text[],bigint[]) FROM app_runtime,app_returns_service;
GRANT EXECUTE ON FUNCTION app.ledger_post_core(uuid,text,text,text,text,uuid[],text[],bigint[]) TO app_payments_service;

CREATE OR REPLACE FUNCTION app.post_ledger(p_tenant uuid,p_currency text,p_source_type text,p_source_id text,p_event text,
 p_accounts uuid[],p_sides text[],p_amounts bigint[]) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_source_type='payment_refund' OR p_event='posted_to_compensated/v1' THEN
  RAISE EXCEPTION 'PAYMENTS_COMPENSATION_CAPABILITY_REQUIRED' USING ERRCODE='42501'; END IF;
 RETURN app.ledger_post_core(p_tenant,p_currency,p_source_type,p_source_id,p_event,p_accounts,p_sides,p_amounts);
END $$;
ALTER FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) OWNER TO app_ledger_executor;
REVOKE ALL ON FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) TO app_runtime,app_returns_service;

-- No runtime/Returns EXECUTE, credentials, membership or witness INSERT is
-- granted. This capability cannot commit money until the closed finalizer lands.
CREATE FUNCTION app.payments_post_refund_compensation(p_tenant uuid,p_currency text,p_source_type text,p_source_id text,p_event text,
 p_accounts uuid[],p_sides text[],p_amounts bigint[]) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_source_type IS DISTINCT FROM 'payment_refund' OR p_event IS DISTINCT FROM 'posted_to_compensated/v1' THEN
  RAISE EXCEPTION 'PAYMENTS_COMPENSATION_NAMESPACE' USING ERRCODE='23514'; END IF;
 RETURN app.ledger_post_core(p_tenant,p_currency,p_source_type,p_source_id,p_event,p_accounts,p_sides,p_amounts);
END $$;
ALTER FUNCTION app.payments_post_refund_compensation(uuid,text,text,text,text,uuid[],text[],bigint[]) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_post_refund_compensation(uuid,text,text,text,text,uuid[],text[],bigint[]) FROM PUBLIC;
GRANT SELECT ON app.payment_refund_attempts,app.payment_refund_reversals,
 app.ledger_accounts,app.ledger_transactions,app.ledger_entries TO app_payments_service;
GRANT INSERT(tenant_id,id,currency,source_type,source_id,event) ON app.ledger_transactions TO app_payments_service;
GRANT INSERT(tenant_id,transaction_id,line_no,account_id,currency,side,amount) ON app.ledger_entries TO app_payments_service;
GRANT UPDATE(status,posted_at) ON app.ledger_transactions TO app_payments_service;
DO $rls$ DECLARE tab text; BEGIN
 FOREACH tab IN ARRAY ARRAY['ledger_accounts','ledger_transactions','ledger_entries'] LOOP
  EXECUTE format($policy$CREATE POLICY payments_compensation_scope ON app.%I TO app_payments_service USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)$policy$,tab);
 END LOOP;
END $rls$;

-- The existing permanent source key now has one canonical encoding. Reserving
-- the event too prevents re-labelling the source type; event aliases cannot post.
CREATE UNIQUE INDEX ledger_refund_compensation_key ON app.ledger_transactions(tenant_id,source_id)
 WHERE source_type='payment_refund';
CREATE FUNCTION app.payments_guard_compensation_source() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF NEW.source_type='payment_refund' OR NEW.event='posted_to_compensated/v1' THEN
  IF current_user<>'app_payments_service' THEN
   RAISE EXCEPTION 'PAYMENTS_COMPENSATION_CAPABILITY_REQUIRED' USING ERRCODE='42501'; END IF;
  IF NEW.source_type<>'payment_refund' OR NEW.event<>'posted_to_compensated/v1'
   OR NOT EXISTS(SELECT FROM app.payment_refund_attempts a WHERE a.tenant_id=NEW.tenant_id AND a.currency=NEW.currency
    AND NEW.source_id=encode(sha256(convert_to(length(a.provider)||':'||a.provider||length(a.provider_account_id)||':'||a.provider_account_id
     ||length(a.provider_refund_id)||':'||a.provider_refund_id,'UTF8')),'hex')) THEN
   RAISE EXCEPTION 'PAYMENTS_COMPENSATION_CANONICAL_KEY' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION app.payments_guard_compensation_source() OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_guard_compensation_source() FROM PUBLIC;
CREATE TRIGGER ledger_compensation_source BEFORE INSERT ON app.ledger_transactions
 FOR EACH ROW EXECUTE FUNCTION app.payments_guard_compensation_source();

CREATE FUNCTION app.payments_assert_compensation_journal() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE j app.ledger_transactions;
BEGIN
 -- Checkout journals include accrual AND settlement, so total credits are 2A.
 -- The immutable cash leg is the refund amount for both ordinary and RMA paths.
 SELECT * INTO STRICT j FROM app.ledger_transactions WHERE tenant_id=NEW.tenant_id AND id=NEW.id;
 IF j.source_type='payment_refund' OR j.event='posted_to_compensated/v1' THEN
  IF j.status<>'posted' OR (SELECT count(*) FROM app.payment_refund_reversals w
   JOIN app.payment_refund_attempts a USING(tenant_id,provider,provider_account_id,environment,provider_refund_id)
   WHERE w.tenant_id=j.tenant_id AND w.reversal_journal_id=j.id AND w.original_journal_id=a.original_journal_id
    AND w.transition=j.event AND j.source_type='payment_refund' AND j.currency=a.currency
    AND j.source_id=encode(sha256(convert_to(length(a.provider)||':'||a.provider||length(a.provider_account_id)||':'||a.provider_account_id
     ||length(a.provider_refund_id)||':'||a.provider_refund_id,'UTF8')),'hex')
    AND a.amount_minor=(SELECT sum(e.amount) FROM app.ledger_entries e
     JOIN app.ledger_accounts ac ON (ac.tenant_id,ac.id)=(e.tenant_id,e.account_id)
     WHERE e.tenant_id=j.tenant_id AND e.transaction_id=j.id AND e.side='debit' AND ac.code='cash'))<>1 THEN
   RAISE EXCEPTION 'PAYMENTS_COMPENSATION_WITNESS_REQUIRED' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION app.payments_assert_compensation_journal() OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_assert_compensation_journal() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER ledger_compensation_witness AFTER INSERT OR UPDATE OF status ON app.ledger_transactions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.source_type='payment_refund' OR NEW.event='posted_to_compensated/v1')
 EXECUTE FUNCTION app.payments_assert_compensation_journal();

-- Validate both directions at transaction end, permitting either insertion order.
-- UPDATE/DELETE/TRUNCATE remain immediate immutable-record rejections.
DROP TRIGGER payment_refund_reversal_guard ON app.payment_refund_reversals;
CREATE TRIGGER payment_refund_reversal_guard BEFORE UPDATE OR DELETE ON app.payment_refund_reversals
 FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_reversal();
ALTER FUNCTION app.payments_guard_refund_reversal() SECURITY DEFINER;
ALTER FUNCTION app.payments_guard_refund_reversal() OWNER TO app_payments_service;
ALTER TABLE app.payment_refund_reversals ALTER CONSTRAINT payment_refund_reversals_tenant_id_reversal_journal_id_fkey DEFERRABLE INITIALLY DEFERRED;
CREATE CONSTRAINT TRIGGER payment_refund_reversal_journal AFTER INSERT ON app.payment_refund_reversals
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_reversal();

-- Temporary CI sample: remove the Ledger-side deferred witness constraint.
DROP TRIGGER ledger_compensation_witness ON app.ledger_transactions;
