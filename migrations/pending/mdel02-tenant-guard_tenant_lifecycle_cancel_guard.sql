-- stable-id: mdel02-tenant-guard
-- depends-on: mdel01-lifecycle
-- module: tenant
-- Responsibility: Tenant-owned destructive cancellation guard before authz.
-- Dependencies: existing lifecycle transition contract and fence; called by the
-- restricted coordinator. Only the phase/CAS guard changes; ACL/owner persist.
-- Invariants: DB clock after lifecycle lock; destructive cancel is a conflict;
-- no new grants, activation or Identity writes. Rollback: forward migration.
CREATE OR REPLACE FUNCTION app.tenant_lifecycle_transition(p_tenant uuid,p_request uuid,p_actor uuid,p_session uuid,
 p_operation text,p_key uuid,p_digest bytea,p_epoch bigint,p_obligations jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE l app.tenant_lifecycles; old app.tenant_lifecycle_receipts; now_db timestamptz;
 result jsonb; category text; blocked boolean:=false; details jsonb:='{}';
 categories text[]:=ARRAY['paid_unfulfilled','fulfillment_return','payment_refund','checkout_capture','refund_reconciliation','pending_payout','unsettled_provider_balance','legal_hold'];
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'tenant context required' USING ERRCODE='42501'; END IF;
 PERFORM app.tenant_lifecycle_transition_fence(p_tenant);
 SELECT * INTO STRICT l FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant;
 now_db:=clock_timestamp();
 IF p_operation IS NULL OR p_operation NOT IN('request','cancel','execute') OR p_request IS NULL THEN
  RAISE EXCEPTION 'invalid lifecycle operation' USING ERRCODE='22023'; END IF;
 -- Tenant owns the phase/CAS guard. Reject destructive cancellation before
 -- consulting interactive Identity authority, which is fenced in this phase.
 IF p_operation='cancel' AND l.phase='executing' THEN
  IF l.request_id IS DISTINCT FROM p_request OR l.epoch IS DISTINCT FROM p_epoch THEN
   RAISE EXCEPTION 'stale lifecycle operation' USING ERRCODE='55000'; END IF;
  RAISE EXCEPTION 'tenant deletion cancellation closed' USING ERRCODE='55000';
 END IF;
 IF p_operation IN('request','cancel') THEN
  IF NOT app.auth_revalidate_tenant(p_tenant,p_actor,'session',p_session,'tenant:delete')
   OR NOT EXISTS(SELECT FROM app.tenant_memberships WHERE tenant_id=p_tenant AND user_id=p_actor AND role='owner' AND status='active') THEN
   RAISE EXCEPTION 'owner session required' USING ERRCODE='42501'; END IF;
  IF p_key IS NULL OR p_digest IS NULL OR octet_length(p_digest)<>32 THEN
   RAISE EXCEPTION 'idempotency key and digest required' USING ERRCODE='22023'; END IF;
  SELECT * INTO old FROM app.tenant_lifecycle_receipts WHERE tenant_id=p_tenant AND id=p_key;
  IF FOUND THEN
   IF old.actor_id IS DISTINCT FROM p_actor OR old.operation IS DISTINCT FROM p_operation
    OR old.request_id IS DISTINCT FROM p_request OR old.body_digest IS DISTINCT FROM p_digest THEN
    RAISE EXCEPTION 'idempotency conflict' USING ERRCODE='23505'; END IF;
   RETURN old.metadata;
  END IF;
 END IF;
 IF p_operation='request' THEN
  IF l.state<>'active' THEN RAISE EXCEPTION 'tenant deletion already active' USING ERRCODE='55000'; END IF;
 ELSE
  IF l.request_id IS DISTINCT FROM p_request OR l.epoch IS DISTINCT FROM p_epoch THEN
   RAISE EXCEPTION 'stale lifecycle operation' USING ERRCODE='55000'; END IF;
 END IF;
 IF p_operation IN('request','execute') THEN
  IF jsonb_typeof(p_obligations) IS DISTINCT FROM 'object' THEN p_obligations:='{}'; END IF;
  IF EXISTS(SELECT FROM jsonb_object_keys(p_obligations) k WHERE NOT k=ANY(categories)) THEN
   RAISE EXCEPTION 'unknown obligation category' USING ERRCODE='22023'; END IF;
  FOREACH category IN ARRAY categories LOOP
   IF NOT p_obligations ? category OR jsonb_typeof(p_obligations->category)<>'number'
    OR (p_obligations->>category)!~'^[0-9]{1,18}$' THEN
    details:=details||jsonb_build_object(category,'unknown'); blocked:=true;
   ELSIF (p_obligations->>category)::bigint<>0 THEN
    details:=details||jsonb_build_object(category,(p_obligations->>category)::bigint); blocked:=true;
   END IF;
  END LOOP;
 END IF;
 IF p_operation='request' AND blocked THEN
  RETURN jsonb_build_object('code','tenant_deletion_blocked','categories',details);
 END IF;
 IF p_operation='request' THEN
  l.state:='cooling_off'; l.phase:='none'; l.epoch:=l.epoch+1;
  l.request_id:=p_request; l.requester_id:=p_actor;
  l.requested_at:=now_db; l.execute_after:=now_db+interval '720 hours'; l.attempt:=0;
  INSERT INTO app.tenant_lifecycle_events(tenant_id,lifecycle_id,request_id,epoch,state,phase,created_at)
   VALUES(p_tenant,p_tenant,p_request,l.epoch,'requested','none',now_db);
 ELSIF p_operation='cancel' THEN
  IF l.state NOT IN('cooling_off','failed') OR l.phase NOT IN('none','preconditions') OR now_db>=l.execute_after THEN
   RAISE EXCEPTION 'tenant deletion cancellation closed' USING ERRCODE='55000'; END IF;
  l.state:='active'; l.phase:='none'; l.epoch:=l.epoch+1;
 ELSE
  IF l.state NOT IN('cooling_off','failed') OR l.phase NOT IN('none','preconditions','executing') OR now_db<l.execute_after
   OR l.next_retry_at IS NOT NULL AND now_db<l.next_retry_at THEN
   RAISE EXCEPTION 'tenant deletion execution closed' USING ERRCODE='55000'; END IF;
  l.epoch:=l.epoch+1; l.attempt:=l.attempt+1;
  IF blocked THEN l.state:='failed';
   IF l.phase<>'executing' THEN l.phase:='preconditions'; END IF;
  ELSE l.state:='executing'; l.phase:='executing'; END IF;
 END IF;
 l.next_retry_at:=CASE WHEN l.state='failed' THEN now_db+interval '60 seconds' END;
 UPDATE app.tenant_lifecycles SET state=l.state,epoch=l.epoch,request_id=l.request_id,requester_id=l.requester_id,
  requested_at=l.requested_at,execute_after=l.execute_after,phase=l.phase,attempt=l.attempt,next_retry_at=l.next_retry_at
 WHERE tenant_id=p_tenant AND id=p_tenant;
 INSERT INTO app.tenant_lifecycle_events(tenant_id,lifecycle_id,request_id,epoch,state,phase,blockers,created_at)
 VALUES(p_tenant,p_tenant,p_request,l.epoch,CASE WHEN p_operation='cancel' THEN 'canceled' ELSE l.state END,l.phase,details,now_db);
 result:=jsonb_build_object('request_id',l.request_id,'state',l.state,'phase',l.phase,'epoch',l.epoch,
  'requested_at',l.requested_at,'execute_after',l.execute_after,'attempt',l.attempt,'next_retry_at',l.next_retry_at,'categories',details);
 IF p_operation IN('request','cancel') THEN
  INSERT INTO app.tenant_lifecycle_receipts(tenant_id,lifecycle_id,id,request_id,actor_id,operation,body_digest,metadata,created_at)
   VALUES(p_tenant,p_tenant,p_key,p_request,p_actor,p_operation,p_digest,result,now_db);
 END IF;
 RETURN result;
END $fn$;
