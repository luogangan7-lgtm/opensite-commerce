-- Responsibility: prove provider namespace separation and Stripe account authority.
-- Dependencies: C-19b forward migration; caller: check_pg. Rollback-only fixtures.
BEGIN;
INSERT INTO app.tenants(name) VALUES('stripe-sandbox-gate') RETURNING id AS t \gset
SELECT set_config('test.stripe_t',:'t',true);
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
 VALUES(:'t','stripe','acct_sqlStripe','test'),(:'t','fake','acct_sqlStripe','test');
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM app.resolve_stripe_payment_account('acct_sqlStripe') WHERE tenant_id=current_setting('test.stripe_t')::uuid AND abs(extract(epoch FROM clock_timestamp()-at_time))<5)
 OR EXISTS(SELECT FROM app.resolve_stripe_payment_account('acct_unknown')) THEN RAISE EXCEPTION 'STRIPE_GATE account authority'; END IF;
END $$;
SELECT app.admit_payment_inbox('acct_sqlStripe','evt_same',sha256('fake'::bytea)) AS fake_id \gset
SELECT set_config('app.payment_provider','stripe',true);
SELECT app.lock_payment_stream('acct_sqlStripe','pi_gate');
SELECT app.admit_payment_inbox('acct_sqlStripe','evt_same',sha256('stripe'::bytea)) AS stripe_id \gset
SELECT set_config('test.stripe_id',:'stripe_id',true),set_config('test.fake_id',:'fake_id',true);
DO $$ BEGIN
 IF current_setting('test.fake_id')=current_setting('test.stripe_id') OR
 app.admit_payment_inbox('acct_sqlStripe','evt_same',sha256('stripe'::bytea))::text<>current_setting('test.stripe_id') THEN RAISE EXCEPTION 'STRIPE_GATE namespace/deduplication'; END IF;
 BEGIN
  PERFORM app.admit_payment_inbox('acct_sqlStripe','evt_same',sha256('changed'::bytea));
  RAISE EXCEPTION 'STRIPE_GATE content identity bypass';
 EXCEPTION WHEN unique_violation THEN NULL; END;
END $$;
RESET ROLE;
UPDATE app.payment_provider_accounts SET enabled=false WHERE tenant_id=:'t' AND provider='stripe';
DO $$ BEGIN IF EXISTS(SELECT FROM app.resolve_stripe_payment_account('acct_sqlStripe')) THEN RAISE EXCEPTION 'STRIPE_GATE disabled account'; END IF; END $$;
ROLLBACK;
SELECT 'PASS: STRIPE_GATE account/DB-clock mapping, provider namespaces and inbox identity';
