-- Responsibility: Payments deletion blockers, ingress and retained witnesses.
-- Dependencies: complete forward schema; disposable PostgreSQL 18 CI only.
-- Invariants: unknown authority never settles; tenant isolation and money survive.
\set ON_ERROR_STOP on
BEGIN;
DO $schema$ BEGIN
 IF to_regprocedure('app.payments_deletion_obligations(uuid)') IS NULL THEN
  RAISE EXCEPTION 'MDEL04_OBLIGATION_PORT_REQUIRED'; END IF;
END $schema$;
INSERT INTO app.tenants(name) VALUES('mdel04-a') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('mdel04-b') RETURNING id AS tb \gset
SELECT set_config('test.mdel04.a',:'ta',true),set_config('test.mdel04.b',:'tb',true);
INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment)
 VALUES(:'ta','fake','mdel04-a','test'),(:'tb','fake','mdel04-b','test');
DO $test$
DECLARE t uuid:=current_setting('test.mdel04.a')::uuid; b uuid:=current_setting('test.mdel04.b')::uuid;
 event uuid; cid uuid; req uuid:=uuidv7(); counts jsonb; kind text; original jsonb; other_original jsonb; n bigint;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 counts:=app.payments_deletion_obligations(t);
 IF counts IS DISTINCT FROM '{"payment_refund":0,"refund_reconciliation":0}'::jsonb THEN RAISE EXCEPTION 'MDEL04_INTERNAL_SETTLED'; END IF;
 FOREACH kind IN ARRAY ARRAY['captured','failed','canceled','partially_refunded','refunded'] LOOP
  event:=app.admit_payment_inbox('mdel04-a','pending-'||kind,sha256(kind::bytea));
  INSERT INTO app.payment_events(tenant_id,id,provider_account_id,provider_ref,event_key,claimed_order_id,order_version,status,amount_minor,currency)
   VALUES(t,event,'mdel04-a','mdel04-ref',encode(sha256(kind::bytea),'hex'),uuidv7(),1,kind,100,'USD');
  counts:=app.payments_deletion_obligations(t);
  IF counts->>'payment_refund'<>'1' THEN RAISE EXCEPTION 'MDEL04_PENDING_KIND %',kind; END IF;
  UPDATE app.payment_events SET applied=true WHERE tenant_id=t AND id=event;
  IF app.payments_deletion_obligations(t)->>'payment_refund'<>'0' THEN RAISE EXCEPTION 'MDEL04_APPLIED_SETTLED %',kind; END IF;
 END LOOP;
 cid:=app.payments_retain_refund_case(jsonb_build_object('account_id','mdel04-a','provider_ref','mdel04-ref','refund_id','mdel04-refund',
  'event_id','case-one','payload_hash',repeat('ab',32),'reason','unmatched_refund'));
 IF app.payments_deletion_obligations(t)->>'refund_reconciliation'<>'1' THEN RAISE EXCEPTION 'MDEL04_UNRESOLVED_CASE'; END IF;
 INSERT INTO app.payment_refund_case_resolutions(tenant_id,case_id,outcome,reason,verifier,evidence_hash)
  VALUES(t,cid,'resolved_no_posting','external_verified',uuidv7(),sha256('reviewed'::bytea));
 IF app.payments_deletion_obligations(t)->>'refund_reconciliation'<>'0' THEN RAISE EXCEPTION 'MDEL04_RESOLVED_CASE'; END IF;
 BEGIN
  PERFORM app.payments_deletion_obligations(b);
  RAISE EXCEPTION 'MDEL04_CROSS_TENANT';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN
  PERFORM app.payments_deletion_obligations(t);
  RAISE EXCEPTION 'MDEL04_MISSING_GUC';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id',t::text,true);
 SELECT jsonb_agg(to_jsonb(e) ORDER BY id) INTO original FROM app.payment_events e WHERE tenant_id=t;
 SELECT to_jsonb(a) INTO other_original FROM app.payment_provider_accounts a WHERE tenant_id=b;
 UPDATE app.tenant_lifecycles SET state='cooling_off',request_id=req,epoch=1,
  requested_at=clock_timestamp(),execute_after=clock_timestamp()+interval '720 hours' WHERE tenant_id=t;
 BEGIN
  INSERT INTO app.payment_events(tenant_id,id,provider_account_id,provider_ref,event_key,claimed_order_id,order_version,status,amount_minor,currency)
   VALUES(t,event,'mdel04-a','new-obligation',repeat('f',64),uuidv7(),1,'captured',100,'USD');
  RAISE EXCEPTION 'MDEL04_COOLING_NEW_OBLIGATION';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing',epoch=2 WHERE tenant_id=t;
 BEGIN
  PERFORM app.payments_erase_merchant_records(t,req,1);
  RAISE EXCEPTION 'MDEL04_STALE_ERASE';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN
  PERFORM app.payments_erase_merchant_records(t,uuidv7(),2);
  RAISE EXCEPTION 'MDEL04_ERASE_REQUEST';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 n:=app.payments_erase_merchant_records(t,req,2);
 IF n<>1 OR EXISTS(SELECT FROM app.payment_provider_accounts WHERE tenant_id=t AND enabled) THEN RAISE EXCEPTION 'MDEL04_DERIVED_ACCOUNT_SCRUB'; END IF;
 IF app.payments_erase_merchant_records(t,req,2)<>0 THEN RAISE EXCEPTION 'MDEL04_ERASE_REPLAY'; END IF;
 IF (SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM app.payment_events e WHERE tenant_id=t) IS DISTINCT FROM original THEN RAISE EXCEPTION 'MDEL04_RETAINED_FACTS'; END IF;
 IF (SELECT to_jsonb(a) FROM app.payment_provider_accounts a WHERE tenant_id=b) IS DISTINCT FROM other_original THEN RAISE EXCEPTION 'MDEL04_OTHER_TENANT'; END IF;
 -- Expose both tenants to the executor in this test only: explicit WHERE is
 -- independent of FORCE RLS. Re-enable target fixture while still superuser.
 UPDATE app.tenant_lifecycles SET state='active',phase='none' WHERE tenant_id=t;
 UPDATE app.payment_provider_accounts SET enabled=true WHERE tenant_id=t;
 UPDATE app.tenant_lifecycles SET state='executing',phase='executing' WHERE tenant_id=t;
 CREATE POLICY mdel04_predicate_witness ON app.payment_provider_accounts TO app_payments_service USING(true) WITH CHECK(true);
 PERFORM app.payments_erase_merchant_records(t,req,2);
 IF (SELECT to_jsonb(a) FROM app.payment_provider_accounts a WHERE tenant_id=b) IS DISTINCT FROM other_original THEN RAISE EXCEPTION 'MDEL04_ERASE_TENANT_PREDICATE'; END IF;
 BEGIN
  PERFORM app.admit_payment_inbox('mdel04-a','late',sha256('late'::bytea));
  INSERT INTO app.payment_refund_cases(tenant_id,provider,provider_account_id,case_key,provider_ref,reason,evidence_hash)
   VALUES(t,'fake','mdel04-a','late','mdel04-ref','unmatched_refund',sha256('late'::bytea));
  RAISE EXCEPTION 'MDEL04_EXECUTION_INGRESS';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $test$;
ROLLBACK;
SELECT 'PASS: MDEL04 internal blockers, ingress fence, tenant-only scrub and retained financial facts';
