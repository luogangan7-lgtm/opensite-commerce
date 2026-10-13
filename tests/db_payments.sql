-- Responsibility: real payment-account authority, inbox identity and grants gate.
-- Dependencies: Payments forward migration and reliability inbox; called by check_pg.
-- Invariants: trusted mapping is unique, disabled/unknown never resolve; rollback fixture.
BEGIN;
SELECT count(*) FROM app.payment_provider_accounts;
SELECT count(*) FROM app.payment_events;
INSERT INTO app.tenants(name) VALUES('payments-account-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('payments-account-B') RETURNING id AS other_t \gset
SELECT set_config('test.payments_t',:'t',true),set_config('test.payments_other_t',:'other_t',true);
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled)
 VALUES(:'t','fake','c19-sql-authority','test',true),(:'other_t','fake','c19-sql-disabled','test',false);
DO $payments$
DECLARE t uuid:=current_setting('test.payments_t')::uuid; mapped uuid; observed timestamptz;
BEGIN
 BEGIN
  INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
  VALUES(t,'fake','another-same-tenant','test');
  RAISE EXCEPTION 'PAYMENTS_GATE ambiguous tenant account admitted';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
  VALUES(t,'fake','c19-live-denied','live');
  RAISE EXCEPTION 'PAYMENTS_GATE live account registered';
 EXCEPTION WHEN check_violation THEN NULL; END;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_payment_resolver' AND
  (rolcanlogin OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR rolinherit))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_payment_resolver'::regrole OR roleid='app_payment_resolver'::regrole) THEN
  RAISE EXCEPTION 'PAYMENTS_GATE unsafe resolver authority';
 END IF;
 SELECT tenant_id,at_time INTO mapped,observed FROM app.resolve_fake_payment_account('c19-sql-authority');
 IF mapped IS DISTINCT FROM t OR observed IS NULL OR abs(extract(epoch FROM clock_timestamp()-observed))>5 THEN
  RAISE EXCEPTION 'PAYMENTS_GATE mapping/database clock differs';
 END IF;
 IF EXISTS(SELECT FROM app.resolve_fake_payment_account('c19-sql-disabled')) OR
  EXISTS(SELECT FROM app.resolve_fake_payment_account('c19-sql-unknown')) THEN
  RAISE EXCEPTION 'PAYMENTS_GATE unknown/disabled resolved';
 END IF;
 BEGIN
  INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled)
   VALUES(current_setting('test.payments_other_t')::uuid,'fake','c19-sql-authority','test',true);
  RAISE EXCEPTION 'PAYMENTS_GATE cross-tenant account alias accepted';
 EXCEPTION WHEN unique_violation THEN NULL; END;
END $payments$;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_runtime;
DO $runtime$
BEGIN
 BEGIN
  INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled)
   VALUES(current_setting('test.payments_t')::uuid,'fake','runtime-injected','test',true);
  RAISE EXCEPTION 'PAYMENTS_GATE runtime forged account authority';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.payment_provider_accounts SET enabled=true WHERE provider_account_id='c19-sql-disabled';
  RAISE EXCEPTION 'PAYMENTS_GATE runtime enabled account';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $runtime$;
INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
 VALUES(:'t','fake','c19-sql-authority','test','same-id',sha256('original'::bytea),'payment:original');
DO $identity$
BEGIN
 BEGIN
  INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,payload_hash,payload_reference)
   VALUES(current_setting('test.payments_t')::uuid,'fake','c19-sql-authority','test','same-id',sha256('changed'::bytea),'payment:changed');
  RAISE EXCEPTION 'PAYMENTS_GATE same identity different hash accepted';
 EXCEPTION WHEN unique_violation THEN NULL; END;
END $identity$;
SELECT set_config('app.tenant_id',:'other_t',true);
DO $isolation$
BEGIN
 IF EXISTS(SELECT FROM app.payment_events WHERE tenant_id=current_setting('test.payments_t')::uuid) OR
  EXISTS(SELECT FROM app.webhook_inbox WHERE provider_account_id='c19-sql-authority') THEN
  RAISE EXCEPTION 'PAYMENTS_GATE cross-tenant events/inbox leaked';
 END IF;
END $isolation$;
RESET ROLE;
ROLLBACK;
SELECT 'payments SQL PASS' AS result;
