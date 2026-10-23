-- stable-id: c19d3-recovery
-- depends-on: c19d2-finalizer
-- Responsibility: retained, append-only refund reconciliation and closed recovery.
-- Dependencies: mapped Inbox, Part 2 finalizer and posted reversal witnesses.
-- Callers: authenticated ingress and staff read; no provider IO or new refund.
CREATE TABLE app.payment_refund_cases (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), id uuid NOT NULL DEFAULT uuidv7(),
 provider text NOT NULL CHECK(provider IN('fake','stripe')), provider_account_id text NOT NULL,
 environment text NOT NULL DEFAULT 'test' CHECK(environment='test'), case_key text NOT NULL,
 provider_refund_id text, provider_ref text, reason text NOT NULL,
 evidence_hash bytea NOT NULL CHECK(octet_length(evidence_hash)=32), created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,provider,provider_account_id,environment,case_key),
 FOREIGN KEY(tenant_id,provider,provider_account_id,environment) REFERENCES app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment),
 CHECK(reason IN('unmatched_refund','refund_terminal_after_match','refund_terminal_retrieval_failed','refund_terminal_unconfirmed','terminal_unbound'))
);
CREATE TABLE app.payment_refund_case_deliveries (
 tenant_id uuid NOT NULL, case_id uuid NOT NULL, inbox_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(tenant_id,inbox_id),
 FOREIGN KEY(tenant_id,case_id) REFERENCES app.payment_refund_cases(tenant_id,id),
 FOREIGN KEY(tenant_id,inbox_id) REFERENCES app.webhook_inbox(tenant_id,id)
);
CREATE TABLE app.payment_refund_case_resolutions (
 tenant_id uuid NOT NULL, case_id uuid NOT NULL, outcome text NOT NULL CHECK(outcome IN('resolved_compensated','resolved_no_posting')),
 reason text NOT NULL CHECK(reason IN('verified_terminal','external_verified','duplicate_confirmed','no_posting_required')),
 verifier uuid, evidence_hash bytea NOT NULL CHECK(octet_length(evidence_hash)=32),
 original_journal_id uuid, reversal_journal_id uuid, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,case_id,outcome), FOREIGN KEY(tenant_id,case_id) REFERENCES app.payment_refund_cases(tenant_id,id),
 FOREIGN KEY(tenant_id,original_journal_id) REFERENCES app.ledger_transactions(tenant_id,id),
 FOREIGN KEY(tenant_id,reversal_journal_id) REFERENCES app.ledger_transactions(tenant_id,id),
 CHECK((reason='verified_terminal' AND verifier IS NULL) OR (reason<>'verified_terminal' AND verifier IS NOT NULL)),
 CHECK((outcome='resolved_compensated' AND original_journal_id IS NOT NULL AND reversal_journal_id IS NOT NULL)
  OR (outcome='resolved_no_posting' AND original_journal_id IS NULL AND reversal_journal_id IS NULL))
);
CREATE INDEX payment_refund_case_age ON app.payment_refund_cases(tenant_id,created_at,id);
CREATE FUNCTION app.payments_guard_refund_case() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'C19D3_APPEND_ONLY' USING ERRCODE='23514'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'C19D3_TENANT' USING ERRCODE='42501'; END IF;
 IF TG_TABLE_NAME='payment_refund_case_resolutions' THEN
 IF NEW.reason='verified_terminal' THEN
  IF NOT EXISTS(SELECT FROM app.payment_refund_cases c JOIN app.payment_refund_terminals f
   ON (f.tenant_id,f.provider,f.provider_account_id,f.environment)=(c.tenant_id,c.provider,c.provider_account_id,c.environment)
   AND (f.provider_refund_id=c.provider_refund_id OR c.provider_refund_id IS NULL AND EXISTS(
    SELECT FROM app.payment_refund_case_deliveries d WHERE (d.tenant_id,d.case_id,d.inbox_id)=(c.tenant_id,c.id,f.inbox_id)))
   WHERE (c.tenant_id,c.id)=(NEW.tenant_id,NEW.case_id) AND f.original_journal_id IS NOT DISTINCT FROM NEW.original_journal_id
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
ALTER FUNCTION app.payments_guard_refund_case() OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_guard_refund_case() FROM PUBLIC;
DO $tables$ DECLARE n text; BEGIN
 FOREACH n IN ARRAY ARRAY['payment_refund_cases','payment_refund_case_deliveries','payment_refund_case_resolutions'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',n);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',n);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_payments_service USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',n);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime',n);
  EXECUTE format('GRANT SELECT,INSERT ON app.%I TO app_payments_service',n);
  EXECUTE format('CREATE TRIGGER refund_case_guard BEFORE INSERT OR UPDATE OR DELETE ON app.%I FOR EACH ROW EXECUTE FUNCTION app.payments_guard_refund_case()',n);
 END LOOP;
END $tables$;
CREATE FUNCTION app.payments_retain_refund_case(p_fact jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid;
 p text:=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake');
 account text:=p_fact->>'account_id'; ref text:=p_fact->>'provider_ref'; refund text:=nullif(p_fact->>'refund_id','');
 event text:=p_fact->>'event_id'; h bytea; k text; inbox uuid; c app.payment_refund_cases;
BEGIN
 IF account IS NULL OR ref IS NULL OR event IS NULL OR p_fact->>'payload_hash' IS NULL OR p_fact->>'reason' IS NULL
  OR account !~ '^[A-Za-z0-9_-]{1,128}$' OR ref !~ '^[!-~]{1,200}$' OR event !~ '^[!-~]{1,128}$'
  OR refund IS NOT NULL AND refund !~ '^[!-~]{1,200}$' OR p_fact->>'payload_hash' !~ '^[a-f0-9]{64}$' THEN
  RAISE EXCEPTION 'C19D3_INVALID_CASE' USING ERRCODE='22023'; END IF;
 IF t IS NULL OR t IS DISTINCT FROM(SELECT tenant_id FROM app.resolve_fake_payment_account(account) WHERE p='fake'
  UNION ALL SELECT tenant_id FROM app.resolve_stripe_payment_account(account) WHERE p='stripe') THEN
  RAISE EXCEPTION 'C19D3_ACCOUNT' USING ERRCODE='42501'; END IF;
 PERFORM app.lock_payment_stream(account,ref);
 h:=decode(p_fact->>'payload_hash','hex'); inbox:=app.admit_payment_inbox(account,event,h);
 SELECT c0.* INTO c FROM app.payment_refund_cases c0 JOIN app.payment_refund_case_deliveries d
  ON (d.tenant_id,d.case_id)=(c0.tenant_id,c0.id) WHERE (d.tenant_id,d.inbox_id)=(t,inbox);
 IF FOUND THEN
  IF c.provider_ref IS NOT NULL AND c.provider_ref<>ref OR c.provider_refund_id IS NOT NULL AND c.provider_refund_id IS DISTINCT FROM refund THEN
   RAISE EXCEPTION 'C19D3_BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
  RETURN c.id;
 END IF;
 k:=CASE WHEN refund IS NULL THEN 'event:'||event ELSE 'refund:'||refund END;
 INSERT INTO app.payment_refund_cases(tenant_id,provider,provider_account_id,case_key,provider_refund_id,provider_ref,reason,evidence_hash)
 VALUES(t,p,account,k,refund,ref,p_fact->>'reason',h) ON CONFLICT DO NOTHING;
 SELECT * INTO STRICT c FROM app.payment_refund_cases WHERE tenant_id=t AND provider=p AND provider_account_id=account AND environment='test' AND case_key=k;
 IF c.provider_ref<>ref THEN RAISE EXCEPTION 'C19D3_BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
 INSERT INTO app.payment_refund_case_deliveries(tenant_id,case_id,inbox_id) VALUES(t,c.id,inbox) ON CONFLICT DO NOTHING;
 IF NOT EXISTS(SELECT FROM app.payment_refund_case_deliveries WHERE tenant_id=t AND case_id=c.id AND inbox_id=inbox) THEN
  RAISE EXCEPTION 'C19D3_DELIVERY_CONFLICT' USING ERRCODE='23505'; END IF;
 PERFORM app.finish_payment_inbox(inbox,false);
 RETURN c.id;
END $$;
ALTER FUNCTION app.payments_retain_refund_case(jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_retain_refund_case(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_retain_refund_case(jsonb) TO app_runtime,app_payments_ingress;

-- Preserve the Part 2 financial implementation as a private function; every
-- caller of the public finalizer now commits its case audit in the same transaction.
ALTER FUNCTION app.payments_finalize_refund(jsonb) RENAME TO payments_finalize_refund_core;
REVOKE ALL ON FUNCTION app.payments_finalize_refund_core(jsonb) FROM app_payments_ingress;
CREATE FUNCTION app.payments_finalize_refund(p_fact jsonb) RETURNS jsonb
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
  INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,evidence_hash,original_journal_id,reversal_journal_id)
  VALUES(c.tenant_id,c.id,CASE WHEN f.reversal_journal_id IS NULL THEN 'resolved_no_posting' ELSE 'resolved_compensated' END,
   'verified_terminal',c.evidence_hash,f.original_journal_id,f.reversal_journal_id) ON CONFLICT DO NOTHING;
  PERFORM app.finish_payment_inbox(f.inbox_id,true);
 END IF;
 RETURN result;
END $$;
ALTER FUNCTION app.payments_finalize_refund(jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_finalize_refund(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_finalize_refund(jsonb) TO app_payments_ingress;

CREATE FUNCTION app.payments_refund_case_evidence(p_event text,p_account text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT jsonb_build_object('case_id',c.id,'event_id',i.provider_event_id,'payload_hash',encode(i.payload_hash,'hex'),
  'provider_ref',c.provider_ref,'refund_id',c.provider_refund_id)
 FROM app.payment_refund_cases c JOIN app.payment_refund_case_deliveries d ON (d.tenant_id,d.case_id)=(c.tenant_id,c.id)
 JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(d.tenant_id,d.inbox_id)
 WHERE c.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND c.provider=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake')
 AND c.provider_account_id=p_account AND i.provider_event_id=p_event;
$$;
ALTER FUNCTION app.payments_refund_case_evidence(text,text) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_refund_case_evidence(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_refund_case_evidence(text,text) TO app_payments_ingress;
CREATE FUNCTION app.payments_replay_refund_case(p_case uuid,p_fact jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE e jsonb;
BEGIN
 e:=app.payments_refund_case_evidence(p_fact->>'event_id',p_fact->>'account_id');
 IF e IS NULL OR (e->>'case_id')::uuid IS DISTINCT FROM p_case
  OR e->>'refund_id' IS NOT NULL AND e->>'refund_id' IS DISTINCT FROM p_fact->>'refund_id'
  OR e->>'provider_ref' IS NOT NULL AND e->>'provider_ref' IS DISTINCT FROM p_fact->>'provider_ref'
  OR e->>'payload_hash' IS DISTINCT FROM p_fact->>'payload_hash' THEN
  RAISE EXCEPTION 'C19D3_REPLAY_BINDING' USING ERRCODE='23514'; END IF;
 -- Only this capability may refresh freshness after provider retrieval. The
 -- original event/hash remain unchanged; current provider verification is external.
 RETURN app.payments_finalize_refund(p_fact||jsonb_build_object('timestamp',extract(epoch FROM clock_timestamp())::bigint));
END $$;
ALTER FUNCTION app.payments_replay_refund_case(uuid,jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_replay_refund_case(uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_replay_refund_case(uuid,jsonb) TO app_payments_ingress;

CREATE FUNCTION app.payments_resolve_refund_case(p_case uuid,p_resolution jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; c app.payment_refund_cases; r app.payment_refund_case_resolutions;
 actor uuid:=(p_resolution->>'user_id')::uuid; reason text:=p_resolution->>'reason'; h bytea;
BEGIN
 IF reason IS NULL OR reason NOT IN('external_verified','duplicate_confirmed','no_posting_required')
  OR p_resolution->>'evidence_hash' IS NULL OR p_resolution->>'evidence_hash' !~ '^[a-f0-9]{64}$'
  OR NOT app.auth_revalidate_tenant(t,actor,p_resolution->>'kind',(p_resolution->>'credential_id')::uuid,'returns:write') THEN
  RAISE EXCEPTION 'C19D3_OPERATOR_DENIED' USING ERRCODE='42501'; END IF;
 SELECT * INTO STRICT c FROM app.payment_refund_cases WHERE tenant_id=t AND id=p_case
  AND provider=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake');
 PERFORM app.lock_payment_stream(c.provider_account_id,c.provider_ref);
 IF EXISTS(SELECT FROM app.payment_refund_case_resolutions WHERE tenant_id=t AND case_id=p_case AND reason='verified_terminal') THEN RETURN; END IF;
 h:=decode(p_resolution->>'evidence_hash','hex');
 INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,verifier,evidence_hash)
 VALUES(t,p_case,'resolved_no_posting',reason,actor,h) ON CONFLICT DO NOTHING;
 SELECT * INTO STRICT r FROM app.payment_refund_case_resolutions WHERE tenant_id=t AND case_id=p_case AND outcome='resolved_no_posting';
 IF (r.verifier,r.reason,r.evidence_hash) IS DISTINCT FROM(actor,reason,h) THEN
  RAISE EXCEPTION 'C19D3_RESOLUTION_CONFLICT' USING ERRCODE='23505'; END IF;
END $$;
ALTER FUNCTION app.payments_resolve_refund_case(uuid,jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_resolve_refund_case(uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_resolve_refund_case(uuid,jsonb) TO app_payments_ingress;
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) TO app_payments_service;

CREATE FUNCTION app.payments_open_refund_cases(p_after uuid DEFAULT NULL) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 WITH open AS (
  SELECT c.id,c.reason,c.created_at FROM app.payment_refund_cases c
  WHERE c.tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND NOT EXISTS(
   SELECT FROM app.payment_refund_case_resolutions r WHERE (r.tenant_id,r.case_id)=(c.tenant_id,c.id)))
 SELECT jsonb_build_object('open_count',(SELECT count(*) FROM open),
  'oldest_age_seconds',coalesce((SELECT greatest(0,extract(epoch FROM statement_timestamp()-min(created_at)))::bigint FROM open),0),
  'items',coalesce((SELECT jsonb_agg(to_jsonb(page) ORDER BY id) FROM(SELECT * FROM open WHERE p_after IS NULL OR id>p_after ORDER BY id LIMIT 100) page),'[]'::jsonb));
$$;
ALTER FUNCTION app.payments_open_refund_cases(uuid) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_open_refund_cases(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_open_refund_cases(uuid) TO app_runtime;

-- Old retained Stripe failures have no raw payload. Preserve exact Inbox IDs/
-- hashes without guessing a refund identity; recovery retrieves the immutable event.
DO $backfill$ DECLARE i record; cid uuid; BEGIN
 FOR i IN SELECT w.*,e.provider_ref,e.provider_refund_id FROM app.webhook_inbox w
  LEFT JOIN app.payment_events e ON (e.tenant_id,e.id)=(w.tenant_id,w.id)
  WHERE w.provider='stripe' AND w.status='failed' AND (e.id IS NULL OR e.status IN('partially_refunded','refunded')) LOOP
  PERFORM set_config('app.tenant_id',i.tenant_id::text,true);
  INSERT INTO app.payment_refund_cases(tenant_id,provider,provider_account_id,case_key,provider_refund_id,provider_ref,reason,evidence_hash)
  VALUES(i.tenant_id,i.provider,i.provider_account_id,CASE WHEN i.provider_refund_id IS NULL THEN 'event:'||i.provider_event_id ELSE 'refund:'||i.provider_refund_id END,
   i.provider_refund_id,i.provider_ref,'unmatched_refund',i.payload_hash) ON CONFLICT DO NOTHING;
  SELECT id INTO STRICT cid FROM app.payment_refund_cases WHERE tenant_id=i.tenant_id AND provider=i.provider
   AND provider_account_id=i.provider_account_id AND case_key=CASE WHEN i.provider_refund_id IS NULL THEN 'event:'||i.provider_event_id ELSE 'refund:'||i.provider_refund_id END;
  INSERT INTO app.payment_refund_case_deliveries(tenant_id,case_id,inbox_id) VALUES(i.tenant_id,cid,i.id);
 END LOOP;
END $backfill$;
