-- stable-id: mdel04-payments
-- depends-on: mdel02-identity
-- depends-on: c19d3-recovery
-- Responsibility: Payments-owned obligation witnesses, ingress fence and account scrub.
-- Dependencies: Tenant lifecycle; immutable Payments facts and case resolutions.
-- Callers: mapped ingress, SQL/bulk writers and dormant DataRights owner ports.
-- Invariants: tenant fence before module locks; no provider IO or money mutation.
-- Upgrade preserves all existing rows; rollback is a forward repair, never history edits.
DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_deletion_executor') THEN
  CREATE ROLE app_payments_deletion_executor NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
 END IF;
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_payments_deletion_executor' AND
  (rolcanlogin OR rolinherit OR rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication))
 OR EXISTS(SELECT FROM pg_auth_members WHERE member='app_payments_deletion_executor'::regrole OR roleid='app_payments_deletion_executor'::regrole) THEN
  RAISE EXCEPTION 'MDEL04_UNSAFE_EXECUTOR'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_payments_deletion_executor;
GRANT SELECT ON app.tenant_lifecycles,app.payment_provider_accounts,app.payment_events,app.payment_event_deliveries,
 app.payment_return_refunds,app.payment_refund_attempts,app.payment_refund_reversals,app.payment_refund_terminals,
 app.payment_refund_cases,app.payment_refund_case_deliveries,app.payment_refund_case_resolutions TO app_payments_deletion_executor;
GRANT UPDATE(enabled) ON app.payment_provider_accounts TO app_payments_deletion_executor;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_read_fence(uuid) TO app_payments_deletion_executor,app_payment_resolver;

-- This witness is deliberately INTERNAL only. Provider payout/balance authority
-- is supplied by the Go ProviderSettlement port and defaults to unknown.
CREATE FUNCTION app.payments_deletion_obligations(p_tenant uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE pending bigint; cases bigint;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 SELECT count(*) INTO pending FROM app.payment_events e WHERE e.tenant_id=p_tenant AND NOT e.applied
  AND NOT EXISTS(SELECT FROM app.payment_refund_case_deliveries d
   JOIN app.payment_refund_case_resolutions r ON (r.tenant_id,r.case_id)=(d.tenant_id,d.case_id)
   WHERE d.tenant_id=e.tenant_id AND (d.inbox_id=e.id OR EXISTS(SELECT FROM app.payment_event_deliveries b
    WHERE b.tenant_id=e.tenant_id AND b.payment_event_id=e.id AND b.id=d.inbox_id)));
 SELECT count(*) INTO cases FROM app.payment_refund_cases c WHERE c.tenant_id=p_tenant
  AND NOT EXISTS(SELECT FROM app.payment_refund_case_resolutions r WHERE r.tenant_id=c.tenant_id AND r.case_id=c.id);
 RETURN jsonb_build_object('payment_refund',pending,'refund_reconciliation',cases);
END $$;

-- Statement trigger locks the tenant before UPDATE/DELETE can acquire a module
-- row lock. Row trigger independently rejects missing/wrong row ownership and
-- new obligations during cooling. SQL capabilities and bulk paths share it.
CREATE FUNCTION app.payments_deletion_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid;
 s text; ph text; v jsonb; refund text; account text; provider_name text; known boolean;
BEGIN
 -- Provisioning is administrative (account INSERT only); it still fences each
 -- row's actual tenant. All application paths require their trusted GUC.
 IF TG_LEVEL='STATEMENT' AND TG_TABLE_NAME='payment_provider_accounts' AND TG_OP='INSERT' THEN RETURN NULL; END IF;
 IF TG_LEVEL='ROW' AND TG_TABLE_NAME='payment_provider_accounts' AND TG_OP='INSERT' THEN t:=NEW.tenant_id; END IF;
 IF t IS NULL THEN RAISE EXCEPTION 'MDEL04_TENANT_CONTEXT' USING ERRCODE='42501'; END IF;
 IF TG_LEVEL='ROW' AND TG_TABLE_NAME='payment_provider_accounts' AND TG_OP='INSERT' THEN
  PERFORM set_config('app.tenant_id',t::text,true);
 END IF;
 PERFORM app.tenant_lifecycle_read_fence(t);
 SELECT state,phase INTO STRICT s,ph FROM app.tenant_lifecycles WHERE tenant_id=t AND id=t;
 IF TG_LEVEL='ROW' THEN
  v:=CASE WHEN TG_OP='DELETE' THEN to_jsonb(OLD) ELSE to_jsonb(NEW) END;
  IF (v->>'tenant_id')::uuid IS DISTINCT FROM t THEN RAISE EXCEPTION 'MDEL04_TENANT_CONTEXT' USING ERRCODE='42501'; END IF;
 END IF;
 IF s='active' THEN
  IF TG_LEVEL='STATEMENT' THEN RETURN NULL; END IF;
  IF TG_OP='DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
 END IF;
 IF s IN('executing','deleted') OR s='failed' AND ph<>'preconditions' THEN
  IF TG_TABLE_NAME='payment_provider_accounts' AND TG_OP='UPDATE' THEN
   IF TG_LEVEL='STATEMENT' THEN RETURN NULL; END IF;
   IF NOT NEW.enabled AND (to_jsonb(NEW)-'enabled')=(to_jsonb(OLD)-'enabled') THEN RETURN NEW; END IF;
  END IF;
  RAISE EXCEPTION 'MDEL04_EXECUTION_FENCE' USING ERRCODE='55000';
 END IF;
 IF s NOT IN('cooling_off','failed') OR TG_TABLE_NAME='payment_provider_accounts' OR TG_OP='DELETE' THEN
  RAISE EXCEPTION 'MDEL04_NEW_OBLIGATION' USING ERRCODE='55000'; END IF;
 IF TG_LEVEL='STATEMENT' THEN RETURN NULL; END IF;
 -- Immutable guards already limit existing outcome updates. Inserts require a
 -- pre-fence Payments identity, never a new arbitrary webhook/refund obligation.
 IF TG_TABLE_NAME='payment_events' AND TG_OP='UPDATE' THEN RETURN NEW; END IF;
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'MDEL04_NEW_OBLIGATION' USING ERRCODE='55000'; END IF;
 IF TG_TABLE_NAME='payment_event_deliveries' THEN
  known:=EXISTS(SELECT FROM app.payment_events WHERE tenant_id=t AND id=NEW.payment_event_id);
 ELSIF TG_TABLE_NAME IN('payment_refund_case_deliveries','payment_refund_case_resolutions') THEN
  known:=EXISTS(SELECT FROM app.payment_refund_cases WHERE tenant_id=t AND id=(v->>'case_id')::uuid);
 ELSE
  refund:=v->>'provider_refund_id'; account:=v->>'provider_account_id'; provider_name:=v->>'provider';
  known:=EXISTS(SELECT FROM app.payment_events e WHERE e.tenant_id=t AND e.provider=provider_name
   AND e.provider_account_id=account AND (refund IS NOT NULL AND e.provider_refund_id=refund
    OR TG_TABLE_NAME='payment_events' AND e.event_key=v->>'event_key'))
   OR EXISTS(SELECT FROM app.payment_refund_attempts a WHERE a.tenant_id=t AND a.provider=provider_name AND a.provider_account_id=account AND a.provider_refund_id=refund)
   OR EXISTS(SELECT FROM app.payment_return_refunds a WHERE a.tenant_id=t AND a.provider=provider_name AND a.provider_account_id=account AND a.provider_refund_id=refund)
   OR EXISTS(SELECT FROM app.payment_refund_cases c WHERE c.tenant_id=t AND c.provider=provider_name AND c.provider_account_id=account AND c.provider_refund_id=refund);
 END IF;
 IF NOT coalesce(known,false) THEN RAISE EXCEPTION 'MDEL04_NEW_OBLIGATION' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION app.payments_erase_merchant_records(p_tenant uuid,p_request uuid,p_epoch bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE n bigint;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 IF NOT EXISTS(SELECT FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant
  AND request_id=p_request AND epoch=p_epoch AND state='executing' AND phase='executing') THEN
  RAISE EXCEPTION 'MDEL04_STALE_ERASE' USING ERRCODE='55000'; END IF;
 UPDATE app.payment_provider_accounts SET enabled=false WHERE tenant_id=p_tenant AND enabled;
 GET DIAGNOSTICS n=ROW_COUNT;
 RETURN n;
END $$;

DO $capabilities$ DECLARE f regprocedure; tab text; BEGIN
 GRANT CREATE ON SCHEMA app TO app_payments_deletion_executor;
 FOREACH f IN ARRAY ARRAY['app.payments_deletion_obligations(uuid)'::regprocedure,
  'app.payments_deletion_guard()'::regprocedure,'app.payments_erase_merchant_records(uuid,uuid,bigint)'::regprocedure] LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_payments_deletion_executor',f);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
 END LOOP;
 REVOKE CREATE ON SCHEMA app FROM app_payments_deletion_executor;
 FOREACH tab IN ARRAY ARRAY['payment_provider_accounts','payment_events','payment_event_deliveries','payment_return_refunds',
  'payment_refund_attempts','payment_refund_reversals','payment_refund_terminals','payment_refund_cases',
  'payment_refund_case_deliveries','payment_refund_case_resolutions'] LOOP
  EXECUTE format('CREATE POLICY payments_deletion_read ON app.%I FOR SELECT TO app_payments_deletion_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',tab);
  EXECUTE format('CREATE TRIGGER a_mdel04_statement BEFORE INSERT OR UPDATE OR DELETE ON app.%I FOR EACH STATEMENT EXECUTE FUNCTION app.payments_deletion_guard()',tab);
  EXECUTE format('CREATE TRIGGER a_mdel04_row BEFORE INSERT OR UPDATE OR DELETE ON app.%I FOR EACH ROW EXECUTE FUNCTION app.payments_deletion_guard()',tab);
 END LOOP;
END $capabilities$;
CREATE POLICY payments_deletion_account_scrub ON app.payment_provider_accounts FOR UPDATE TO app_payments_deletion_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT EXECUTE ON FUNCTION app.payments_deletion_obligations(uuid) TO app_runtime;
-- No application role can invoke erasure; all-module activation composes the
-- restricted work item later. Trigger execution needs no caller EXECUTE grant.

-- Account mapping must take the tenant fence BEFORE locking an account row.
-- Preserve its trusted mapping, account lock, test-mode and DB-time semantics.
DO $mapping$ DECLARE f record; definition text; body text; provider_name text; BEGIN
 FOR f IN SELECT oid,proname,prosrc FROM pg_proc WHERE pronamespace='app'::regnamespace
  AND proname IN('resolve_fake_payment_account','resolve_stripe_payment_account') LOOP
  provider_name:=CASE f.proname WHEN 'resolve_fake_payment_account' THEN 'fake' ELSE 'stripe' END;
  body:=format($body$DECLARE t uuid; BEGIN
   SELECT a.tenant_id INTO t FROM app.payment_provider_accounts a
    WHERE a.provider=%L AND a.environment='test' AND a.provider_account_id=p_account AND a.enabled;
   IF NOT FOUND THEN RETURN; END IF;
   PERFORM set_config('app.tenant_id',t::text,true);
   PERFORM app.tenant_lifecycle_read_fence(t);
   RETURN QUERY SELECT a.tenant_id,clock_timestamp() FROM app.payment_provider_accounts a
    WHERE a.tenant_id=t AND a.provider=%L AND a.environment='test' AND a.provider_account_id=p_account AND a.enabled FOR SHARE OF a;
  END$body$,provider_name,provider_name);
  definition:=pg_get_functiondef(f.oid);
  definition:=replace(definition,'LANGUAGE sql','LANGUAGE plpgsql');
  definition:=replace(definition,f.prosrc,body);
  EXECUTE definition;
 END LOOP;
END $mapping$;
