-- Responsibility: runtime privacy regressions; collect every named defect before failing.
-- Dependencies: data-rights, orders and quote-conversion controlled commands on disposable PG18.
-- Caller: check_pg.sh. All synthetic fixture writes and snapshot probes roll back.
-- Invariants: bounded erasure, coherent snapshot, trusted ownership, terminal exports.
\set ON_ERROR_STOP on
BEGIN;
CREATE TEMP TABLE privacy_failures(label text);
CREATE FUNCTION pg_temp.privacy_check(label text,command text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 BEGIN EXECUTE command;
 EXCEPTION WHEN OTHERS THEN
  INSERT INTO privacy_failures VALUES(label||': '||SQLSTATE||' '||SQLERRM);
  RAISE NOTICE 'PRIVACY_HARDENING_FAIL %: % %',label,SQLSTATE,SQLERRM;
 END;
END $$;
INSERT INTO app.tenants(name) VALUES('privacy-hardening') RETURNING id AS t \gset
SELECT set_config('app.tenant_id',:'t',true);
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','bounded@example.invalid') RETURNING id AS bounded \gset
INSERT INTO app.data_rights_exports(tenant_id,id,customer_id,status)
 SELECT :'t',uuidv7(),:'bounded','revoked' FROM generate_series(1,121);
SELECT set_config('test.privacy_bounded',:'bounded',true);
SELECT pg_temp.privacy_check('bounded-erasure',$test$
 DO $$ DECLARE j jsonb; BEGIN
  SET LOCAL ROLE app_runtime;
  j:=app.data_rights_erase_customer(current_setting('test.privacy_bounded')::uuid,'service',uuidv7());
  IF jsonb_array_length(j->'requests')>100 THEN RAISE EXCEPTION 'erase returned unbounded export IDs'; END IF;
  RESET ROLE;
  IF (SELECT count(*) FROM app.data_rights_exports WHERE customer_id=current_setting('test.privacy_bounded')::uuid AND status='revoked')<>121 THEN
   RAISE EXCEPTION 'durable cleanup rows lost'; END IF;
 END $$ $test$);
-- A side effect deliberately appears between count/assembly statements. It is
-- invisible to the active command snapshot, visible to the next command. This
-- deterministic snapshot probe replaces timing-dependent concurrent sleeps.
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','snapshot@example.invalid') RETURNING id AS snapshot \gset
SELECT set_config('test.privacy_snapshot',:'snapshot',true),set_config('test.privacy_injected','false',true);
INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
 VALUES(:'t',:'snapshot','snapshot','snapshot','snapshot','US');
CREATE FUNCTION pg_temp.privacy_snapshot_inject(customer uuid) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
 IF customer=current_setting('test.privacy_snapshot')::uuid AND current_setting('test.privacy_injected')='false' THEN
  PERFORM set_config('test.privacy_injected','true',true);
  INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
   SELECT current_setting('app.tenant_id')::uuid,customer,'snapshot','snapshot','snapshot','US' FROM generate_series(1,10001);
 END IF;
 RETURN true;
END $$;
CREATE POLICY privacy_snapshot_probe ON app.customer_addresses AS RESTRICTIVE TO app_data_rights_executor
 USING(pg_temp.privacy_snapshot_inject(customer_id));
SELECT pg_temp.privacy_check('same-snapshot',$test$
 DO $$ DECLARE j jsonb;r uuid:=uuidv7(); BEGIN
  SET LOCAL ROLE app_runtime;
  PERFORM app.data_rights_request_export(r,current_setting('test.privacy_snapshot')::uuid,'service',uuidv7());
  j:=app.data_rights_build_export(r);
  IF jsonb_array_length(j->'addresses') IS DISTINCT FROM 1 THEN RAISE EXCEPTION 'count and assembly used different snapshots'; END IF;
  RESET ROLE;
 END $$ $test$);
DROP POLICY privacy_snapshot_probe ON app.customer_addresses;
DELETE FROM app.customer_addresses WHERE customer_id=:'snapshot';
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','limit@example.invalid') RETURNING id AS limited \gset
SELECT set_config('test.privacy_limit',:'limited',true);
INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
 SELECT :'t',:'limited','limit','limit','limit','US' FROM generate_series(1,10001);
SELECT pg_temp.privacy_check('terminal-record-limit',$test$
 DO $$ DECLARE j jsonb;r uuid:=uuidv7(); BEGIN
  SET LOCAL ROLE app_runtime;
  PERFORM app.data_rights_request_export(r,current_setting('test.privacy_limit')::uuid,'service',uuidv7());
  j:=app.data_rights_build_export(r);
  IF j->>'status' IS DISTINCT FROM 'failed' THEN RAISE EXCEPTION 'limit has no terminal sentinel'; END IF;
  j:=app.data_rights_status(r,current_setting('test.privacy_limit')::uuid);
  IF j->>'status' IS DISTINCT FROM 'failed' THEN RAISE EXCEPTION 'limit failed state not durable'; END IF;
  RESET ROLE;
 END $$ $test$);
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','expiry@example.invalid') RETURNING id AS expiry \gset
INSERT INTO app.data_rights_exports(tenant_id,id,customer_id,expires_at) VALUES(:'t',uuidv7(),:'expiry',clock_timestamp()-interval '1 second') RETURNING id AS expired \gset
SELECT set_config('test.privacy_expired',:'expired',true),set_config('test.privacy_expiry_customer',:'expiry',true);
SELECT pg_temp.privacy_check('terminal-expiry',$test$
 DO $$ DECLARE j jsonb; BEGIN
  SET LOCAL ROLE app_runtime;
  j:=app.data_rights_worker_info(current_setting('test.privacy_expired')::uuid);
  IF j->>'status' IS DISTINCT FROM 'expired' THEN RAISE EXCEPTION 'expiry has no terminal sentinel'; END IF;
  j:=app.data_rights_status(current_setting('test.privacy_expired')::uuid,current_setting('test.privacy_expiry_customer')::uuid);
  IF j->>'status' IS DISTINCT FROM 'expired' THEN RAISE EXCEPTION 'expiry state not durable'; END IF;
  RESET ROLE;
 END $$ $test$);
SELECT pg_temp.privacy_check('status-capability-proof',$test$
 DO $$ DECLARE j jsonb;r uuid:=uuidv7();c uuid:=current_setting('test.privacy_expiry_customer')::uuid; BEGIN
  SET LOCAL ROLE app_runtime;
  PERFORM app.data_rights_request_export(r,c,'service',uuidv7());
  PERFORM app.data_rights_complete_export(r,sha256('old-key'::bytea),sha256('content'::bytea),app.data_rights_download_deadline(r));
  j:=app.data_rights_status(r,c);
  IF j->>'token_hash' IS DISTINCT FROM encode(sha256('old-key'::bytea),'hex') OR j->>'download_available' IS DISTINCT FROM 'true' THEN
   RAISE EXCEPTION 'status cannot verify persisted capability'; END IF;
  RESET ROLE;
 END $$ $test$);
-- Guest input must never act as an erasure authority or an unchecked boolean.
SELECT pg_temp.privacy_check('guest-forged-marker',$test$
 DO $$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;o uuid:=uuidv7();e uuid;d app.notification_deliveries; BEGIN
  SET LOCAL ROLE app_orders_executor;
  INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
   VALUES(t,o,901,'8c2cbd72-a0c3-567b-b78b-a1ce4d69d5d5',sha256('forged'::bytea),'{"email":"guest@example.invalid","erased":true}','USD',100,0,0,'{}',100);
  RESET ROLE;
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.paid',jsonb_build_object('order_id',o),'orders.events','privacy-forged') RETURNING id INTO e;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
  IF d.recipient<>'guest@example.invalid' OR d.status<>'pending' THEN RAISE EXCEPTION 'guest forged erasure authority'; END IF;
 END $$ $test$);
SELECT pg_temp.privacy_check('guest-malformed-marker',$test$
 DO $$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;o uuid:=uuidv7();e uuid;d app.notification_deliveries; BEGIN
  SET LOCAL ROLE app_orders_executor;
  INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
   VALUES(t,o,902,'55226d05-0e70-5365-93c3-34cafdd36408',sha256('malformed'::bytea),'{"email":"guest@example.invalid","erased":"anything"}','USD',100,0,0,'{}',100);
  RESET ROLE;
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.paid',jsonb_build_object('order_id',o),'orders.events','privacy-malformed') RETURNING id INTO e;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
  IF d.recipient<>'guest@example.invalid' OR d.status<>'pending' THEN RAISE EXCEPTION 'malformed guest marker affected fanout'; END IF;
 END $$ $test$);
-- quote.converted ownership follows the durable conversion receipt.
SELECT pg_temp.privacy_check('conversion-ownership-and-future-fanout',$test$
 DO $$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;c uuid;a uuid;u uuid;org uuid;s uuid;i uuid;co uuid;q uuid;o uuid:=uuidv7();contact text:='privacy-converter-'||uuidv7()::text||'@canary.invalid';e uuid;nid uuid:=uuidv7();dispatch uuid:=uuidv7();d app.notification_deliveries; BEGIN
  INSERT INTO app.customers(tenant_id,email) VALUES(t,contact) RETURNING id INTO c;
  INSERT INTO app.customers(tenant_id,email) VALUES(t,'acceptor@example.invalid') RETURNING id INTO a;
  INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@staff.invalid','fixture') RETURNING id INTO u;
  INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'admin');
  INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'privacy') RETURNING id INTO org;
  INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,org,'privacy') RETURNING id INTO s;
  INSERT INTO app.buyer_companies(tenant_id,name) VALUES(t,'privacy') RETURNING id INTO co;
  INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES(t,co,c,'approver'),(t,co,a,'purchaser');
  INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message) VALUES(t,s,'inquiry@example.invalid','privacy') RETURNING id INTO i;
  SET LOCAL ROLE app_runtime;
  q:=app.create_quote(t,s,i,co,u,'USD',100,0,0,100,'{}',clock_timestamp()+interval '1 hour','[{"line_no":1,"description":"privacy","quantity":1,"unit_price":100,"line_total":100,"snapshot":{}}]');
  PERFORM app.transition_quote(t,q,1,'send','staff',u);
  PERFORM app.transition_quote(t,q,2,'accept','customer',a);
  PERFORM app.transition_quote(t,q,3,'approve','customer',c);
  RESET ROLE;
  SET LOCAL ROLE app_orders_executor;
  INSERT INTO app.orders(tenant_id,id,customer_id,order_number,creation_key,request_hash,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
   VALUES(t,o,c,903,'b3b57ff6-9864-521b-97e1-15bcf06347ff',sha256('conversion'::bytea),'USD',100,0,0,'{}',100);
  SET LOCAL ROLE app_quotes_executor;
  INSERT INTO app.quote_conversions(tenant_id,quote_id,buyer_company_id,customer_id,order_id,quote_version,idempotency_key,request_hash,order_number,currency,total_minor)
   VALUES(t,q,co,c,o,4,'b3b57ff6-9864-521b-97e1-15bcf06347ff',sha256('conversion'::bytea),903,'USD',100);
  RESET ROLE;
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'quote.converted',jsonb_build_object('quote_id',q,'order_id',o,'quote_version',4),'quotes','converted:'||q) RETURNING id INTO e;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
  IF d.recipient<>contact THEN RAISE EXCEPTION 'conversion fixture contact'; END IF;
  SET LOCAL ROLE app_runtime;
  PERFORM app.data_rights_erase_customer(c,'service',uuidv7());
  RESET ROLE;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
  IF d.status<>'dead_letter' OR d.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR d.recipient<>'erased-'||d.id||'@invalid.test' THEN
   RAISE EXCEPTION 'conversion notification retained PII'; END IF;
  IF EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND recipient=contact)
   OR NOT EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND id=d.dispatch_event_id AND status='dead_letter' AND last_error='PRIVACY_ERASED') THEN
   RAISE EXCEPTION 'conversion canary or dispatch retained'; END IF;
  INSERT INTO app.notification_deliveries(tenant_id,id,source_event_id,dispatch_event_id,recipient,locale,template_key,reference_id)
   VALUES(t,nid,e,dispatch,'late-converter@example.invalid','en','quote.converted',q) RETURNING * INTO d;
  INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,dispatch,'notification.dispatch',jsonb_build_object('notification_id',nid),'notifications.email',nid::text||':1');
  IF d.status<>'dead_letter' OR d.last_error IS DISTINCT FROM 'PRIVACY_ERASED' THEN RAISE EXCEPTION 'conversion future fanout resurrected'; END IF;
 END $$ $test$);
-- A real erased mailbox-linked order remains blocked after its proof is deleted.
-- Removing the new typed receipt insert must make this runtime check fail.
SELECT pg_temp.privacy_check('linked-guest-durable-authority',$test$
 DO $$ DECLARE t uuid:=current_setting('app.tenant_id')::uuid;c uuid;o uuid:=uuidv7();e uuid;d app.notification_deliveries; BEGIN
  INSERT INTO app.customers(tenant_id,email) VALUES(t,'linked-guest@example.invalid') RETURNING id INTO c;
  SET LOCAL ROLE app_orders_executor;
  INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
   VALUES(t,o,904,'1a67ebd0-cb77-53a7-a3c3-4bd64ddab247',sha256('linked'::bytea),'{"email":"linked-guest@example.invalid"}','USD',100,0,0,'{}',100);
  RESET ROLE;
  INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id) VALUES(t,c,o);
  SET LOCAL ROLE app_runtime;
  PERFORM app.data_rights_erase_customer(c,'service',uuidv7());
  RESET ROLE;
  IF EXISTS(SELECT FROM app.customer_guest_order_links WHERE tenant_id=t AND order_id=o) THEN RAISE EXCEPTION 'mailbox proof retained'; END IF;
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.paid',jsonb_build_object('order_id',o),'orders.events','privacy-linked-late') RETURNING id INTO e;
  SELECT * INTO STRICT d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=e;
  IF d.status<>'dead_letter' OR d.last_error IS DISTINCT FROM 'PRIVACY_ERASED' THEN RAISE EXCEPTION 'linked guest late fanout resurrected'; END IF;
 END $$ $test$);
DO $$ DECLARE failures text; BEGIN
 SELECT string_agg(label,E'\n' ORDER BY label) INTO failures FROM privacy_failures;
 IF failures IS NOT NULL THEN RAISE EXCEPTION 'PRIVACY_HARDENING_GATE% %',E'\n',failures; END IF;
END $$;
ROLLBACK;
\echo 'PRIVACY_HARDENING_GATE PASS'
