-- stable-id: c19b-stripe
-- depends-on: c19a-webhook
-- Responsibility: extend existing provider facts/inbox to explicit Stripe TEST mode.
-- Dependencies: C-19a account authority and inbox. No new domain state or live mode.
ALTER TABLE app.payment_provider_accounts DROP CONSTRAINT payment_provider_accounts_provider_check;
ALTER TABLE app.payment_provider_accounts ADD CHECK(provider IN('fake','stripe'));
ALTER TABLE app.payment_events DROP CONSTRAINT payment_events_provider_check;
ALTER TABLE app.payment_events ADD CHECK(provider IN('fake','stripe'));
-- The mapped runner sets this local value only after resolving a Stripe account.
-- Existing Fake transactions omit it and retain their exact historical behavior.
ALTER TABLE app.payment_events ALTER COLUMN provider SET DEFAULT coalesce(nullif(current_setting('app.payment_provider',true),''),'fake');
CREATE FUNCTION app.resolve_stripe_payment_account(p_account text) RETURNS TABLE(tenant_id uuid,at_time timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT a.tenant_id,clock_timestamp() FROM app.payment_provider_accounts a
 WHERE a.provider='stripe' AND a.environment='test' AND a.provider_account_id=p_account AND a.enabled FOR SHARE OF a
$$;
ALTER FUNCTION app.resolve_stripe_payment_account(text) OWNER TO app_payment_resolver;
REVOKE ALL ON FUNCTION app.resolve_stripe_payment_account(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.resolve_stripe_payment_account(text) TO app_runtime;
CREATE OR REPLACE FUNCTION app.lock_payment_stream(p_account text,p_ref text) RETURNS timestamptz LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('payments:'||coalesce(nullif(current_setting('app.payment_provider',true),''),'fake')||':test:'||p_account||':'||p_ref,19));
 RETURN clock_timestamp();
END $$;
CREATE OR REPLACE FUNCTION app.admit_payment_inbox(p_account text,p_event text,p_hash bytea) RETURNS uuid
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid := nullif(current_setting('app.tenant_id',true),'')::uuid; r app.webhook_inbox; new_id uuid := uuidv7();
 p text := coalesce(nullif(current_setting('app.payment_provider',true),''),'fake');
BEGIN
 INSERT INTO app.webhook_inbox(tenant_id,id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(t,new_id,p,p_account,'test',p_event,p_hash,'payment-event:'||new_id::text)
 ON CONFLICT(tenant_id,provider,provider_account_id,environment,provider_event_id) DO NOTHING;
 SELECT * INTO STRICT r FROM app.webhook_inbox WHERE tenant_id=t AND provider=p AND provider_account_id=p_account AND environment='test' AND provider_event_id=p_event FOR UPDATE;
 IF r.payload_hash IS DISTINCT FROM p_hash THEN RAISE EXCEPTION 'payment event content conflict' USING ERRCODE='23505'; END IF;
 RETURN r.id;
END $$;
