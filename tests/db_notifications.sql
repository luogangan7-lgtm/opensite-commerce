-- Responsibility: actual PG notification admission, authoritative fanout and fences.
-- Dependencies: migrated app schemas; check_pg runner. All fixtures roll back.
-- Invariants: own-target claims, no runtime writes, tenant-scoped redrive receipts.
\set ON_ERROR_STOP on
BEGIN;
DO $admission$ BEGIN
 IF to_regclass('app.notification_deliveries') IS NULL OR to_regclass('app.notification_redrive_requests') IS NULL THEN RAISE EXCEPTION 'NOTIFICATIONS_SCHEMA_ADMISSION'; END IF;
END $admission$;
INSERT INTO app.tenants(name) VALUES('notifications-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('notifications-B') RETURNING id AS other_t \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('test.notification_t',:'t',true),set_config('test.notification_other',:'other_t',true);
SET LOCAL ROLE app_orders_executor;
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
VALUES(:'t',uuidv7(),1,'51b14265-cc1c-50ba-bc1f-34ac7512d782',sha256('n'::bytea),'{"email":" Buyer@Example.Test "}','USD',100,0,0,'{}',100) RETURNING id AS oid \gset
INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
VALUES(:'t',uuidv7(),2,'46b5005a-643f-52bd-861d-1dca69ed2907',sha256('n'::bytea),'{"email":"invalid"}','USD',100,0,0,'{}',100) RETURNING id AS invalid_oid \gset
RESET ROLE;
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(:'t','order.paid',jsonb_build_object('order_id',:'oid','recipient','attacker@example.test'),'orders.events','source') RETURNING id AS source \gset
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(:'t','order.paid',jsonb_build_object('order_id',:'invalid_oid'),'orders.events','invalid-source');
INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(:'t','unrelated','{}','other.module','unrelated');
SELECT set_config('test.notification_source',:'source',true);
-- Populated upgrades contain other tenants; fixture counts must stay tenant-scoped.
DO $fanout$ BEGIN
 IF (SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=current_setting('test.notification_t')::uuid AND recipient='buyer@example.test' AND template_key='order.paid')<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE authoritative recipient/fanout'; END IF;
 IF (SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=current_setting('test.notification_t')::uuid AND last_error='NOTIFICATION_RECIPIENT_UNAVAILABLE' AND recipient='' AND dispatch_event_id IS NULL)<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE invalid recipient must persist failure'; END IF;
 IF (SELECT count(*) FROM app.outbox_events WHERE tenant_id=current_setting('test.notification_t')::uuid AND delivery_target='notifications.email')<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE recursive fanout'; END IF;
END $fanout$;
-- Blank terminal contacts must always carry their explicit failure code (NULL is not valid).
DO $contact_null$ DECLARE t uuid:=current_setting('test.notification_t')::uuid; source uuid:=current_setting('test.notification_source')::uuid; BEGIN
 BEGIN
  INSERT INTO app.notification_deliveries(tenant_id,source_event_id,recipient,template_key,reference_id,status,last_error)
  VALUES(t,source,'','order.paid',uuidv7(),'dead_letter',NULL);
  RAISE EXCEPTION 'NOTIFICATIONS_CONTACT_NULL_ERROR accepted missing failure code';
 EXCEPTION WHEN check_violation THEN NULL; END;
END $contact_null$;
-- Renderer admission must agree with persisted/fanout envelopes. Savepoint
-- keeps these witnesses out of the independent claim/fencing tests below.
SAVEPOINT recipient_admission;
DO $recipient_admission$
DECLARE t uuid:=current_setting('test.notification_t')::uuid; contact text; oid uuid; source uuid; d app.notification_deliveries; n integer:=2; valid boolean;
 domain text:=repeat('b',63)||'.'||repeat('c',63)||'.'||repeat('d',56)||'.test';
BEGIN
 FOREACH contact IN ARRAY ARRAY[repeat('a',64)||'@'||domain,repeat('a',64)||'@x'||domain,'.buyer@example.test','buyer..name@example.test','buyer.@example.test','buyer.name@example.test','buyer@.example.test','buyer@example..test','buyer@example.test.'] LOOP
  n:=n+1;valid:=n IN(3,8);
  EXECUTE 'SET LOCAL ROLE app_orders_executor';
  INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
  VALUES(t,uuidv7(),n,'recipient-admission-'||n,sha256('n'::bytea),jsonb_build_object('email',contact),'USD',100,0,0,'{}',100) RETURNING id INTO oid;
  EXECUTE 'RESET ROLE';
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
  VALUES(t,'order.paid',jsonb_build_object('order_id',oid),'orders.events','recipient-admission-'||n) RETURNING id INTO source;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=source;
  IF valid THEN
   IF d.recipient<>contact OR d.status<>'pending' OR d.dispatch_event_id IS NULL THEN RAISE EXCEPTION 'NOTIFICATIONS_RECIPIENT_ADMISSION valid dot atom/254 rejected';END IF;
   IF NOT EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND id=d.dispatch_event_id AND delivery_target='notifications.email') THEN RAISE EXCEPTION 'NOTIFICATIONS_RECIPIENT_ADMISSION valid dispatch missing';END IF;
  ELSE
   IF d.recipient<>'' OR d.status<>'dead_letter' OR d.dispatch_event_id IS NOT NULL OR d.last_error IS DISTINCT FROM 'NOTIFICATION_RECIPIENT_UNAVAILABLE' THEN RAISE EXCEPTION 'NOTIFICATIONS_RECIPIENT_ADMISSION invalid length/dot contact dispatched case %',n;END IF;
   BEGIN PERFORM app.notification_redrive(t,d.id,'6eac585d-53d9-5ad8-86c8-58968c4adb72');RAISE EXCEPTION 'NOTIFICATIONS_RECIPIENT_ADMISSION invalid contact redriven';EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL;END;
   -- Exercise the table CHECK independently of the fanout classification.
   BEGIN
    UPDATE app.notification_deliveries SET recipient=contact,dispatch_event_id=current_setting('test.notification_source')::uuid,status='pending',last_error=NULL WHERE tenant_id=t AND id=d.id;
    RAISE EXCEPTION 'NOTIFICATIONS_RECIPIENT_ADMISSION CHECK accepted invalid contact case %',n;
   EXCEPTION WHEN check_violation THEN NULL;END;
  END IF;
 END LOOP;
END $recipient_admission$;
ROLLBACK TO SAVEPOINT recipient_admission;
SET LOCAL ROLE app_runtime;
DO $runtime$ DECLARE t uuid:=current_setting('test.notification_t')::uuid; BEGIN
 BEGIN INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'notification.dispatch','{}','notifications.email','forge');RAISE EXCEPTION 'NOTIFICATIONS_GATE runtime namespace write'; EXCEPTION WHEN insufficient_privilege THEN NULL;END;
 BEGIN INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'quotes.approved','{}','quotes','forge-quote');RAISE EXCEPTION 'NOTIFICATIONS_GATE runtime quote namespace write';EXCEPTION WHEN insufficient_privilege THEN NULL;END;
 BEGIN UPDATE app.notification_deliveries SET status='sent';RAISE EXCEPTION 'NOTIFICATIONS_GATE runtime delivery update';EXCEPTION WHEN insufficient_privilege THEN NULL;END;
 BEGIN DELETE FROM app.notification_redrive_requests;RAISE EXCEPTION 'NOTIFICATIONS_GATE runtime receipt delete';EXCEPTION WHEN insufficient_privilege THEN NULL;END;
 BEGIN PERFORM app.notification_begin(t,uuidv7(),uuidv7());RAISE EXCEPTION 'NOTIFICATIONS_GATE runtime worker function';EXCEPTION WHEN insufficient_privilege THEN NULL;END;
END $runtime$;
SELECT set_config('app.tenant_id',:'other_t',true);
DO $rls$ BEGIN IF EXISTS(SELECT FROM app.notification_deliveries) THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE cross tenant read';END IF;END $rls$;
SELECT set_config('app.tenant_id','',true);
DO $rls$ BEGIN IF EXISTS(SELECT FROM app.notification_deliveries) THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE empty tenant read';END IF;END $rls$;
SELECT set_config('app.tenant_id',:'t',true);
SET LOCAL ROLE app_outbox_worker;
DO $send$ DECLARE t uuid:=current_setting('test.notification_t')::uuid; e app.outbox_events;d app.notification_deliveries; worker uuid:=uuidv7(); BEGIN
 SELECT * INTO STRICT e FROM app.claim_notification_outbox(t,worker,10,60);
 IF e.delivery_target<>'notifications.email' OR e.attempts<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE own claim';END IF;
 PERFORM set_config('app.outbox_worker',worker::text,true),set_config('app.outbox_attempt',e.attempts::text,true);
 d:=app.notification_begin(t,e.id,(e.payload->>'notification_id')::uuid);
 IF d.status<>'sending' OR d.attempt_count<>1 OR d.send_token IS NULL THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE begin fence';END IF;
 IF app.notification_finish(t,d.id,uuidv7(),'sent',NULL) THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE wrong token finish';END IF;
 IF NOT app.notification_finish(t,d.id,d.send_token,'sent',NULL) THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE sent finish';END IF;
 d:=app.notification_begin(t,e.id,d.id);
 IF d.status<>'sent' OR d.send_token IS NOT NULL THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE sent replay';END IF;
END $send$;
RESET ROLE;
DO $caps$ DECLARE t uuid:=current_setting('test.notification_t')::uuid; source uuid:=current_setting('test.notification_source')::uuid; d app.notification_deliveries; e app.outbox_events; r app.notification_redrive_requests; replay app.notification_redrive_requests;worker uuid:=uuidv7(); BEGIN
 -- New supported event produces a separate notification for fencing/crash gates.
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) SELECT t,'order.fulfilled',payload,'orders.events','shipped' FROM app.outbox_events WHERE tenant_id=t AND id=source;
 SELECT * INTO STRICT e FROM app.claim_notification_outbox(t,worker,10,60);
 PERFORM set_config('app.outbox_worker',worker::text,true),set_config('app.outbox_attempt',e.attempts::text,true);
 d:=app.notification_begin(t,e.id,(e.payload->>'notification_id')::uuid);
 -- Simulate process crash after SMTP starts; a recovered lease cannot resend.
 UPDATE app.outbox_events SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=t AND id=e.id;
 SELECT * INTO STRICT e FROM app.claim_notification_outbox(t,worker,10,60);
 PERFORM set_config('app.outbox_attempt',e.attempts::text,true);
 d:=app.notification_begin(t,e.id,d.id);
 IF d.status<>'uncertain' OR d.send_token IS NOT NULL OR d.attempt_count<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE crash uncertain';END IF;
 BEGIN PERFORM app.notification_redrive(t,d.id,'f9af01df-8dcb-5edb-9989-5df0900bb189');RAISE EXCEPTION 'NOTIFICATIONS_GATE uncertain redrive';EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL;END;
 UPDATE app.outbox_events SET status='delivered',lease_owner=NULL,lease_until=NULL,delivered_at=clock_timestamp() WHERE tenant_id=t AND id=e.id;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) SELECT t,'order.refunded',payload,'orders.events','refund' FROM app.outbox_events WHERE tenant_id=t AND id=source;
 SELECT * INTO STRICT e FROM app.claim_notification_outbox(t,worker,10,60);
 UPDATE app.outbox_events SET attempts=10 WHERE tenant_id=t AND id=e.id;
 PERFORM set_config('app.outbox_attempt','10',true);
 d:=app.notification_begin(t,e.id,(e.payload->>'notification_id')::uuid);
 IF NOT app.notification_finish(t,d.id,d.send_token,'failed','DELIVERY_REJECTED') THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE final failed finish';END IF;
 IF (SELECT status FROM app.notification_deliveries WHERE tenant_id=t AND id=d.id)<>'dead_letter' THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE attempt cap';END IF;
 PERFORM app.fail_outbox(t,e.id,worker,10,'DELIVERY_REJECTED',0);
 r:=app.notification_redrive(t,d.id,'6bb8f30d-acf0-547f-8109-b7cc2be58fd7');replay:=app.notification_redrive(t,d.id,'6bb8f30d-acf0-547f-8109-b7cc2be58fd7');
 IF r IS DISTINCT FROM replay OR r.generation<>2 THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE redrive replay';END IF;
 BEGIN PERFORM app.notification_redrive(t,d.id,'be202eab-408e-5251-8976-139077193bae');RAISE EXCEPTION 'NOTIFICATIONS_GATE redrive nonterminal';EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL;END;
 IF EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND delivery_target IN('orders.events','other.module') AND (status<>'pending' OR attempts<>0)) THEN RAISE EXCEPTION 'NOTIFICATIONS_GATE other target consumed';END IF;
 BEGIN UPDATE app.notification_deliveries SET attempt_count=-1 WHERE tenant_id=t AND id=d.id;RAISE EXCEPTION 'NOTIFICATIONS_GATE negative attempts';EXCEPTION WHEN check_violation THEN NULL;END;
 BEGIN UPDATE app.notification_deliveries SET source_event_id=uuidv7() WHERE tenant_id=t AND id=d.id;RAISE EXCEPTION 'NOTIFICATIONS_GATE source FK';EXCEPTION WHEN foreign_key_violation THEN NULL;END;
END $caps$;
-- Conditional compatibility gate uses actual quote tables and their guards;
-- absence is explicitly NOT_RUN, never a substitute-schema PASS.
DO $quote_compat$
DECLARE t uuid:=current_setting('test.notification_t')::uuid; org uuid;site uuid;company uuid;staff uuid;customer uuid;inquiry uuid;qid uuid;badsource uuid; stale_source uuid;approved uuid; BEGIN
 IF to_regclass('app.quotes') IS NULL THEN
  RAISE NOTICE 'NOT_RUN: NOTIFICATIONS_QUOTE_COMPAT requires actual quotes module';RETURN;
 END IF;
 IF NOT has_column_privilege('app_notifications_executor','app.quotes','status','SELECT') OR NOT has_column_privilege('app_notifications_executor','app.quotes','version','SELECT') THEN
  RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_COMPAT integration grants missing';
 END IF;
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'Notification quote fixture') RETURNING id INTO org;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,org,'Notification quote fixture') RETURNING id INTO site;
 INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@safe.test','fixture-only') RETURNING id INTO staff;
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,staff,'owner');
 INSERT INTO app.buyer_companies(tenant_id,name) VALUES(t,'Notification quote buyer') RETURNING id INTO company;
 INSERT INTO app.customers(tenant_id,email) VALUES(t,'accepted@safe.test') RETURNING id INTO customer;
 INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES(t,company,customer,'purchaser');
 EXECUTE 'INSERT INTO app.inquiries(tenant_id,site_id,buyer_company_id,buyer_email,message) VALUES($1,$2,$3,'' Inquiry@Safe.Test '',''quote test'') RETURNING id' INTO inquiry USING t,site,company;
 EXECUTE 'SET LOCAL ROLE app_quotes_executor';
 EXECUTE 'INSERT INTO app.quotes(tenant_id,site_id,inquiry_id,buyer_company_id,created_by,currency,subtotal,tax,shipping,total,snapshot,expires_at) VALUES($1,$2,$3,$4,$5,''USD'',100,0,0,100,''{}'',clock_timestamp()+interval ''1 day'') RETURNING id' INTO qid USING t,site,inquiry,company,staff;
 EXECUTE 'RESET ROLE';
 -- A forged approved source on a draft is retained but causes no notification.
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'quotes.approved',jsonb_build_object('quote_id',qid,'version',1,'recipient','injected@example.com'),'quotes','quote-wrong-state') RETURNING id INTO badsource;
 IF EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=badsource) THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_SOURCE_AUTH wrong status accepted';END IF;
 EXECUTE 'SET LOCAL ROLE app_quotes_executor';
 EXECUTE 'UPDATE app.quotes SET status=''sent'',version=2 WHERE tenant_id=$1 AND id=$2' USING t,qid;
 EXECUTE 'RESET ROLE';
 IF (SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=t AND reference_id=qid AND template_key='quote.sent' AND recipient='inquiry@safe.test')<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_COMPAT sent authoritative inquiry';END IF;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'quotes.sent',jsonb_build_object('quote_id',qid,'version',1),'quotes','quote-stale-version') RETURNING id INTO stale_source;
 IF EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=stale_source) THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_SOURCE_AUTH stale version accepted';END IF;
 -- With the same true sent lifecycle, mismatch alone must block dispatch and
 -- produce an actionable contact failure instead of selecting another company.
 UPDATE app.inquiries SET buyer_company_id=NULL WHERE tenant_id=t AND id=inquiry;
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'quotes.sent',jsonb_build_object('quote_id',qid,'version',2),'quotes','quote-contact-mismatch') RETURNING id INTO badsource;
 IF NOT EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=badsource AND recipient='' AND status='dead_letter' AND dispatch_event_id IS NULL AND last_error='NOTIFICATION_RECIPIENT_UNAVAILABLE') THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_COMPAT inquiry company mismatch';END IF;
 UPDATE app.inquiries SET buyer_company_id=company WHERE tenant_id=t AND id=inquiry;

 EXECUTE 'SET LOCAL ROLE app_quotes_executor';
 EXECUTE 'UPDATE app.quotes SET status=''accepted_pending_approval'',version=3,accepted_by=$3 WHERE tenant_id=$1 AND id=$2' USING t,qid,customer;
 EXECUTE 'UPDATE app.quotes SET status=''approved'',version=4 WHERE tenant_id=$1 AND id=$2' USING t,qid;
 EXECUTE 'RESET ROLE';
 IF (SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=t AND reference_id=qid AND template_key='quote.approved' AND recipient='accepted@safe.test')<>1 THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_COMPAT approved authenticated customer';END IF;
 -- Already approved quote also cannot admit another sent lifecycle source.
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'quotes.sent',jsonb_build_object('quote_id',qid),'quotes','quote-late-sent') RETURNING id INTO badsource;
 IF EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=badsource) THEN RAISE EXCEPTION 'NOTIFICATIONS_QUOTE_SOURCE_AUTH wrong state without version';END IF;
 RAISE NOTICE 'PASS: NOTIFICATIONS_QUOTE_COMPAT actual quote guards, sent/approved fanout and lifecycle/version negatives';
END $quote_compat$;
ROLLBACK;
SELECT 'PASS: NOTIFICATIONS_GATE authoritative source, own claims, RLS, token fences, uncertain and redrive receipts' AS result;
