-- Temporary CI-only fault probe; never merge.
CREATE OR REPLACE FUNCTION app.notification_redrive(p_tenant uuid,p_notification_id uuid,p_key text) RETURNS app.notification_redrive_requests LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.notification_deliveries;r app.notification_redrive_requests; dispatch uuid:=uuidv7(); BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'NOTIFICATIONS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 IF p_key IS NULL OR length(p_key) NOT BETWEEN 1 AND 128 OR btrim(p_key)='' THEN RAISE EXCEPTION 'NOTIFICATIONS_KEY_ARGUMENT' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=p_tenant AND id=p_notification_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'NOTIFICATIONS_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO r FROM app.notification_redrive_requests WHERE tenant_id=p_tenant AND notification_id=p_notification_id AND idempotency_key=p_key;
 IF FOUND THEN
  -- FAULT PROBE ONLY: incorrectly creates a second delivery for an old key.
  UPDATE app.notification_deliveries SET status='pending',generation=generation+1,dispatch_event_id=dispatch,send_token=NULL,lease_attempt=NULL,lease_worker=NULL,last_error=NULL,updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=p_notification_id RETURNING * INTO d;
  INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key) VALUES(p_tenant,dispatch,'notification.dispatch',jsonb_build_object('notification_id',d.id),'notifications.email',d.id::text||':'||d.generation::text);
  RETURN r;
 END IF;
 IF d.status<>'dead_letter' OR d.recipient='' THEN RAISE EXCEPTION 'NOTIFICATIONS_REDRIVE_STATE' USING ERRCODE='55000'; END IF;
 UPDATE app.notification_deliveries SET status='pending',generation=generation+1,dispatch_event_id=dispatch,send_token=NULL,lease_attempt=NULL,lease_worker=NULL,last_error=NULL,updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=p_notification_id RETURNING * INTO d;
 INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key) VALUES(p_tenant,dispatch,'notification.dispatch',jsonb_build_object('notification_id',d.id),'notifications.email',d.id::text||':'||d.generation::text);
 INSERT INTO app.notification_redrive_requests(tenant_id,notification_id,idempotency_key,generation,dispatch_event_id) VALUES(p_tenant,d.id,p_key,d.generation,dispatch) RETURNING * INTO r;
 RETURN r;
END $$;
