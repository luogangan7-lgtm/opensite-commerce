-- stable-id: b-ledger-audit
-- depends-on: c19dp2-completeness
-- Responsibility: preserve independent staff and provider-verified audit facts.
-- Dependencies: append-only refund cases and the existing payment stream lock.
-- Callers: existing staff resolution/finalizer/replay ports; no money writes.
-- Compatibility: existing facts, evidence and DB timestamps remain byte-for-byte;
-- one manual decision and one verified terminal per case, regardless of outcome.
-- Rollback: forward repair only; never delete or rewrite financial audit history.
ALTER TABLE app.payment_refund_case_resolutions
 DROP CONSTRAINT payment_refund_case_resolutions_pkey,
 ADD PRIMARY KEY(tenant_id,case_id,reason);
CREATE UNIQUE INDEX payment_refund_case_manual_identity
 ON app.payment_refund_case_resolutions(tenant_id,case_id)
 WHERE reason<>'verified_terminal';

-- Both arrival orders append their own fact. The partial unique index retains
-- the one-manual-decision contract; SELECT addresses only that identity so a
-- no-posting verified terminal can never make INTO STRICT ambiguous.
CREATE OR REPLACE FUNCTION app.payments_resolve_refund_case(p_case uuid,p_resolution jsonb) RETURNS void
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
 h:=decode(p_resolution->>'evidence_hash','hex');
 INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,verifier,evidence_hash)
 VALUES(t,p_case,'resolved_no_posting',reason,actor,h) ON CONFLICT DO NOTHING;
 SELECT x.* INTO STRICT r FROM app.payment_refund_case_resolutions x WHERE x.tenant_id=t AND x.case_id=p_case AND x.reason<>'verified_terminal';
 IF (r.verifier,r.reason,r.evidence_hash) IS DISTINCT FROM(actor,reason,h) THEN
  RAISE EXCEPTION 'C19D3_RESOLUTION_CONFLICT' USING ERRCODE='23505'; END IF;
END $$;
ALTER FUNCTION app.payments_resolve_refund_case(uuid,jsonb) OWNER TO app_payments_service;
REVOKE ALL ON FUNCTION app.payments_resolve_refund_case(uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.payments_resolve_refund_case(uuid,jsonb) TO app_payments_ingress;
