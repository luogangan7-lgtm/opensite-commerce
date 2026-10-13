-- Real PostgreSQL privacy gate; synthetic tenant/customer fixtures roll back.
-- Depends on 026 notifications, 027 quotes and 028 controlled functions. Called by check_pg; no production data.
-- Asserts scope, credentials exclusion, single use, erasure and runtime fences.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('privacy-A','USD') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('privacy-B') RETURNING id AS foreign_t \gset
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','erasure-subject@example.invalid','Secret Person') RETURNING id AS c \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'foreign_t','foreign@example.invalid') RETURNING id AS foreign_c \gset
INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
 VALUES(:'t',:'c','Secret Person','Private street','Private city','US');
INSERT INTO app.customer_credentials VALUES(:'t',:'c','$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA',now());
SELECT set_config('app.tenant_id',:'t',true),set_config('test.privacy_customer',:'c',true);
-- Real sealed publication has no customer ownership and must survive erasure.
DO $media$
DECLARE t uuid:=current_setting('test.privacy_seed_t',true)::uuid; o uuid;site uuid;r uuid;b uuid;a uuid;
BEGIN
 t:=(SELECT tenant_id FROM app.customers WHERE email='erasure-subject@example.invalid');
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'Privacy fixture') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'Privacy fixture') RETURNING id INTO site;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,site,1,'test','{}',sha256('manifest'::bytea)) RETURNING id INTO r;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status) VALUES(t,'local','privacy-unowned',sha256('media'::bytea),1,'ready') RETURNING id INTO b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility) VALUES(t,b,'public-unowned','image/png','public') RETURNING id INTO a;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(t,site,r,a);
 PERFORM set_config('test.privacy_release',r::text,true);PERFORM set_config('test.privacy_site',site::text,true);
 PERFORM set_config('test.privacy_media',a::text,true);PERFORM set_config('test.privacy_blob',b::text,true);
END $media$;
SET LOCAL ROLE app_publisher;
SELECT app.publish_release(:'t',current_setting('test.privacy_site')::uuid,current_setting('test.privacy_release')::uuid,NULL);
RESET ROLE;
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','privacy') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','privacy-sku','default') RETURNING id AS sku \gset
SELECT set_config('test.privacy_sku',:'sku',true);
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES(:'t','privacy-asset','USD','asset') RETURNING id AS debit \gset
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES(:'t','privacy-revenue','USD','revenue') RETURNING id AS credit \gset
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',5,'privacy synthetic fixture',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
SELECT app.post_ledger(:'t','USD','order',:'c','paid',ARRAY[:'debit'::uuid,:'credit'::uuid],ARRAY['debit','credit'],ARRAY[130,130]::bigint[]);
SELECT set_config('test.privacy_ledger_before',(SELECT jsonb_agg(to_jsonb(l) ORDER BY transaction_id,line_no)::text FROM app.ledger_entries l),true);
DO $order$
DECLARE sku uuid:=current_setting('test.privacy_sku')::uuid;oid uuid:=uuidv7();r app.inventory_reservations;o app.orders;l jsonb;
BEGIN
 r:=app.inventory_reserve(sku,1,'order',oid,'privacy-reserve',900);
 l:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','frozen-sku','quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,'reservation_id',r.id,'tax_snapshot',jsonb_build_object('net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD','basis_points','Secret Person','private','Private street')));
 o:=app.orders_create(oid,'privacy-create',sha256('fixture'::bytea),current_setting('test.privacy_customer')::uuid,NULL,'USD',20,'{"amount_minor":20,"currency":"USD","recipient":"Secret Person","address":"Private street"}',l,'[]');
 PERFORM set_config('test.privacy_order',oid::text,true);
 PERFORM set_config('test.privacy_order_before',(to_jsonb(o)-'shipping_snapshot')::text,true);
 PERFORM set_config('test.privacy_line_before',(SELECT (to_jsonb(x)-'tax_snapshot')::text FROM app.order_lines x WHERE order_id=oid),true);
END $order$;
RESET ROLE;
-- Frozen recipients, an admitted send, a historical send, and old redrive generations.
DO $notification_seed$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; oid uuid:=current_setting('test.privacy_order')::uuid; d app.notification_deliveries; i integer; source uuid; w uuid:=uuidv7();
BEGIN
 FOR i IN 1..4 LOOP
  INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
   VALUES(t,'order.paid',jsonb_build_object('order_id',oid),'orders.events','privacy-notification-'||i) RETURNING id INTO source;
  SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=source;
  IF d.recipient<>'erasure-subject@example.invalid' THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION fixture recipient'; END IF;
  IF i=1 THEN
   UPDATE app.outbox_events SET status='processing',lease_owner=w,lease_until=clock_timestamp()+interval '5 minutes',attempts=1 WHERE tenant_id=t AND id=d.dispatch_event_id;
   UPDATE app.notification_deliveries SET status='sending',send_token=uuidv7(),lease_worker=w,lease_attempt=1 WHERE tenant_id=t AND id=d.id RETURNING * INTO d;
   PERFORM set_config('test.privacy_notification',d.id::text,true);PERFORM set_config('test.privacy_dispatch',d.dispatch_event_id::text,true);
   PERFORM set_config('test.privacy_send_token',d.send_token::text,true);PERFORM set_config('test.privacy_send_worker',w::text,true);
  ELSIF i=2 THEN
   UPDATE app.notification_deliveries SET status='sent',sent_at=clock_timestamp() WHERE tenant_id=t AND id=d.id;
   UPDATE app.outbox_events SET status='delivered',delivered_at=clock_timestamp() WHERE tenant_id=t AND id=d.dispatch_event_id;
  ELSIF i=3 THEN
   UPDATE app.notification_deliveries SET status='dead_letter',last_error='SEND_FAILED' WHERE tenant_id=t AND id=d.id;
   PERFORM app.notification_redrive(t,d.id,'privacy-old-redrive');
  END IF;
 END LOOP;
 -- Identical email is deliberately not an ownership key: an unrelated guest
 -- order's frozen recipient must remain intact.
 EXECUTE 'SET LOCAL ROLE app_orders_executor';
 INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor)
  VALUES(t,uuidv7(),2,'privacy-unowned-guest',sha256('guest'::bytea),'{"email":"erasure-subject@example.invalid"}','USD',100,0,0,'{}',100) RETURNING id INTO oid;
 EXECUTE 'RESET ROLE';
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
  VALUES(t,'order.paid',jsonb_build_object('order_id',oid),'orders.events','privacy-unowned-guest') RETURNING id INTO source;
 SELECT id INTO source FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=source;
 PERFORM set_config('test.privacy_unowned_notification',source::text,true);
 PERFORM set_config('test.privacy_unowned_order',oid::text,true);
END $notification_seed$;
-- Quote approval mail is owned by quotes.accepted_by. One quote is approved
-- before erasure; a second accepted quote is approved only after erasure.
DO $quote_seed$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; c uuid:=current_setting('test.privacy_customer')::uuid;
 site uuid:=current_setting('test.privacy_site')::uuid; u uuid; a uuid; co uuid; i uuid; q uuid; n integer;
BEGIN
 INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@staff.invalid','fixture') RETURNING id INTO u;
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,u,'admin');
 INSERT INTO app.customers(tenant_id,email) VALUES(t,'privacy-approver@example.invalid') RETURNING id INTO a;
 INSERT INTO app.buyer_companies(tenant_id,name) VALUES(t,'Privacy buyer') RETURNING id INTO co;
 INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES(t,co,c,'purchaser'),(t,co,a,'approver');
 INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message) VALUES(t,site,'privacy-desk@example.invalid','privacy quote') RETURNING id INTO i;
 EXECUTE 'SET LOCAL ROLE app_runtime';
 FOR n IN 1..2 LOOP
  q:=app.create_quote(t,site,i,co,u,'USD',100,0,0,100,'{}',clock_timestamp()+interval '1 hour','[{"line_no":1,"description":"privacy","quantity":1,"unit_price":100,"line_total":100,"snapshot":{}}]');
  PERFORM app.transition_quote(t,q,1,'send','staff',u);
  PERFORM app.transition_quote(t,q,2,'accept','customer',c);
  IF n=1 THEN PERFORM app.transition_quote(t,q,3,'approve','customer',a);PERFORM set_config('test.privacy_quote',q::text,true);
  ELSE PERFORM set_config('test.privacy_pending_quote',q::text,true); END IF;
 END LOOP;
 EXECUTE 'RESET ROLE';
 PERFORM set_config('test.privacy_approver',a::text,true);
 IF NOT EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND template_key='quote.approved' AND reference_id=current_setting('test.privacy_quote')::uuid AND recipient='erasure-subject@example.invalid' AND status='pending') THEN
  RAISE EXCEPTION 'PRIVACY_QUOTE fixture approval recipient'; END IF;
END $quote_seed$;
-- The erased-placeholder namespace is reserved: no account may pre-register a
-- victim's placeholder (any case) and thereby make erasure fail on uniqueness.
DO $reserved_namespace$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; c uuid:=current_setting('test.privacy_customer')::uuid; e text;
BEGIN
 FOREACH e IN ARRAY ARRAY['erased+'||c::text||'@erased.invalid','ERASED+'||c::text||'@Erased.Invalid',' erased+'||c::text||'@erased.invalid','other@erased.invalid'] LOOP
  BEGIN INSERT INTO app.customers(tenant_id,email) VALUES(t,e); RAISE EXCEPTION 'PRIVACY_GATE reserved namespace squatted %',e; EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN UPDATE app.customers SET email=e WHERE tenant_id=t AND email='privacy-approver@example.invalid'; RAISE EXCEPTION 'PRIVACY_GATE reserved namespace update %',e; EXCEPTION WHEN check_violation THEN NULL; END;
 END LOOP;
END $reserved_namespace$;
SELECT set_config('app.tenant_id',:'t',true),set_config('test.privacy_customer',:'c',true),set_config('test.privacy_foreign',:'foreign_c',true);
SET LOCAL ROLE app_runtime;
DO $gate$
DECLARE c uuid:=current_setting('test.privacy_customer')::uuid; r uuid:=uuidv7(); j jsonb; x jsonb;
BEGIN
 j:=app.data_rights_request_export(r,c,'user',uuidv7());
 IF j->>'status'<>'pending' THEN RAISE EXCEPTION 'PRIVACY_GATE pending'; END IF;
 x:=app.data_rights_build_export(r);
 IF x::text LIKE '%password_hash%' OR x::text LIKE '%argon2id%' OR x::text LIKE '%foreign@example%' THEN
  RAISE EXCEPTION 'PRIVACY_GATE sensitive export'; END IF;
 IF x->'customer'->>'display_name'<>'Secret Person' OR jsonb_array_length(x->'addresses')<>1 THEN
  RAISE EXCEPTION 'PRIVACY_GATE incomplete export'; END IF;
 PERFORM app.data_rights_complete_export(r,sha256('token'::bytea),sha256(x::text::bytea),app.data_rights_download_deadline(r));
 PERFORM app.data_rights_consume_export(r,c,sha256('token'::bytea));
 BEGIN PERFORM app.data_rights_consume_export(r,c,sha256('token'::bytea)); RAISE EXCEPTION 'PRIVACY_GATE replay'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM app.data_rights_request_export(uuidv7(),current_setting('test.privacy_foreign')::uuid,'user',uuidv7());
  RAISE EXCEPTION 'PRIVACY_GATE cross tenant'; EXCEPTION WHEN no_data_found THEN NULL; END;
 r:=uuidv7();PERFORM app.data_rights_request_export(r,c,'service',uuidv7());
 x:=app.data_rights_erase_customer(c,'user',uuidv7());
 IF (SELECT jsonb_agg(to_jsonb(l) ORDER BY transaction_id,line_no)::text FROM app.ledger_entries l)<>current_setting('test.privacy_ledger_before') THEN RAISE EXCEPTION 'PRIVACY_GATE ledger changed'; END IF;
 IF (SELECT (to_jsonb(o)-'shipping_snapshot')::text FROM app.orders o WHERE id=current_setting('test.privacy_order')::uuid)<>current_setting('test.privacy_order_before') OR (SELECT (to_jsonb(l)-'tax_snapshot')::text FROM app.order_lines l WHERE order_id=current_setting('test.privacy_order')::uuid)<>current_setting('test.privacy_line_before') THEN RAISE EXCEPTION 'PRIVACY_GATE financial identity changed'; END IF;
 IF (SELECT shipping_snapshot::text FROM app.orders WHERE id=current_setting('test.privacy_order')::uuid) LIKE '%Secret%' OR (SELECT tax_snapshot FROM app.order_lines WHERE order_id=current_setting('test.privacy_order')::uuid)<>'{"erased":true}'::jsonb THEN RAISE EXCEPTION 'PRIVACY_GATE snapshot PII residue'; END IF;
 IF x->'counts'->>'credentials'<>'1' OR x->'counts'->>'addresses'<>'1' THEN RAISE EXCEPTION 'PRIVACY_GATE erase counts'; END IF;
 IF (SELECT status FROM app.customers WHERE id=c)<>'disabled' OR (SELECT display_name FROM app.customers WHERE id=c)<>'[erased]' THEN
  RAISE EXCEPTION 'PRIVACY_GATE customer barrier'; END IF;
 BEGIN PERFORM app.data_rights_build_export(r); RAISE EXCEPTION 'PRIVACY_GATE revoked build'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM app.data_rights_request_export(uuidv7(),c,'user',uuidv7()); RAISE EXCEPTION 'PRIVACY_GATE disabled export'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM password_hash FROM app.customer_credentials; RAISE EXCEPTION 'PRIVACY_GATE runtime credentials'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN DELETE FROM app.customer_credentials; RAISE EXCEPTION 'PRIVACY_GATE runtime delete'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.data_rights_exports SET status='ready'; RAISE EXCEPTION 'PRIVACY_GATE raw export mutation'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $gate$;
RESET ROLE;
DO $notification_privacy$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; oid uuid:=current_setting('test.privacy_order')::uuid; n uuid:=current_setting('test.privacy_notification')::uuid; source uuid; nd app.notification_deliveries;
BEGIN
 IF (SELECT recipient FROM app.notification_deliveries WHERE tenant_id=t AND id=current_setting('test.privacy_unowned_notification')::uuid)<>'erasure-subject@example.invalid' THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION unowned email matched'; END IF;
 IF EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND reference_id=oid AND (recipient<>'erased-'||id::text||'@invalid.test' OR status<>'dead_letter' OR last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR send_token IS NOT NULL OR lease_worker IS NOT NULL OR lease_attempt IS NOT NULL OR sent_at IS NOT NULL)) THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION PII or live send residue'; END IF;
 IF EXISTS(SELECT FROM app.outbox_events e JOIN app.notification_deliveries d ON d.tenant_id=e.tenant_id AND e.payload=jsonb_build_object('notification_id',d.id) WHERE d.tenant_id=t AND d.reference_id=oid AND (e.status NOT IN('dead_letter','delivered') OR e.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR e.lease_owner IS NOT NULL OR e.lease_until IS NOT NULL)) THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION live dispatch generation'; END IF;
 BEGIN PERFORM app.notification_redrive(t,n,'privacy-revive');RAISE EXCEPTION 'PRIVACY_NOTIFICATION redrive revival'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 EXECUTE 'SET LOCAL ROLE app_runtime';
 BEGIN UPDATE app.notification_deliveries SET recipient='resurrected@example.test' WHERE tenant_id=t AND id=n; RAISE EXCEPTION 'PRIVACY_NOTIFICATION raw mutation'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_notifications_executor';
 BEGIN UPDATE app.notification_deliveries SET status='pending',last_error=NULL WHERE tenant_id=t AND id=n; RAISE EXCEPTION 'PRIVACY_NOTIFICATION terminal revival'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 EXECUTE 'RESET ROLE';
 EXECUTE 'SET LOCAL ROLE app_data_rights_executor';
 BEGIN UPDATE app.notification_deliveries SET recipient='forged@example.test' WHERE tenant_id=t AND id=n; RAISE EXCEPTION 'PRIVACY_NOTIFICATION forged scrub'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.outbox_events SET status='pending',last_error=NULL WHERE tenant_id=t AND id=current_setting('test.privacy_dispatch')::uuid; RAISE EXCEPTION 'PRIVACY_NOTIFICATION dispatch revival'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 EXECUTE 'RESET ROLE';
 -- A financial event after erasure remains valid, but cannot revive a frozen contact.
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(t,'order.fulfilled',jsonb_build_object('order_id',oid),'orders.events','privacy-future-notification') RETURNING id INTO source;
 SELECT * INTO nd FROM app.notification_deliveries WHERE tenant_id=t AND source_event_id=source;
 IF nd.status<>'dead_letter' OR nd.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR nd.recipient<>'erased-'||nd.id::text||'@invalid.test' THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION future fanout'; END IF;
 IF EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND id=nd.dispatch_event_id AND status<>'dead_letter') THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION future dispatch'; END IF;
 IF (SELECT email FROM app.customers WHERE tenant_id=t AND id=current_setting('test.privacy_customer')::uuid)<>'erased+'||current_setting('test.privacy_customer')||'@erased.invalid' THEN RAISE EXCEPTION 'PRIVACY_GATE reserved placeholder'; END IF;
 -- Quote approval notifications: scrubbed, dead-lettered, and a post-erasure
 -- approval of a quote the customer accepted cannot revive the contact.
 IF NOT EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND reference_id=current_setting('test.privacy_quote')::uuid AND template_key='quote.approved')
 OR EXISTS(SELECT FROM app.notification_deliveries WHERE tenant_id=t AND reference_id=current_setting('test.privacy_quote')::uuid AND template_key='quote.approved' AND (recipient<>'erased-'||id::text||'@invalid.test' OR status<>'dead_letter' OR last_error IS DISTINCT FROM 'PRIVACY_ERASED'))
 OR EXISTS(SELECT FROM app.outbox_events e JOIN app.notification_deliveries d ON d.tenant_id=e.tenant_id AND e.payload=jsonb_build_object('notification_id',d.id) WHERE d.tenant_id=t AND d.reference_id=current_setting('test.privacy_quote')::uuid AND (e.status<>'dead_letter' OR e.last_error IS DISTINCT FROM 'PRIVACY_ERASED')) THEN
  RAISE EXCEPTION 'PRIVACY_QUOTE approval notification residue'; END IF;
 EXECUTE 'SET LOCAL ROLE app_runtime';
 PERFORM app.transition_quote(t,current_setting('test.privacy_pending_quote')::uuid,3,'approve','customer',current_setting('test.privacy_approver')::uuid);
 EXECUTE 'RESET ROLE';
 SELECT * INTO nd FROM app.notification_deliveries WHERE tenant_id=t AND template_key='quote.approved' AND reference_id=current_setting('test.privacy_pending_quote')::uuid;
 IF nd.id IS NULL OR nd.status<>'dead_letter' OR nd.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR nd.recipient<>'erased-'||nd.id::text||'@invalid.test'
 OR EXISTS(SELECT FROM app.outbox_events WHERE tenant_id=t AND id=nd.dispatch_event_id AND status<>'dead_letter') THEN RAISE EXCEPTION 'PRIVACY_QUOTE post-erasure approval revived contact'; END IF;
END $notification_privacy$;
-- No leftover PII anywhere: every row of every non-system table (all modules,
-- including notifications/outbox/DLQ and quotes) is searched for the erased
-- customer's email, name and address. Only the deliberately unowned guest
-- order and its notification may still carry the same email.
DO $no_leftover_pii$
DECLARE r record; hits bigint; found text:='';
BEGIN
 FOR r IN SELECT n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE c.relkind IN('r','p') AND n.nspname NOT IN('pg_catalog','information_schema') AND NOT starts_with(n.nspname,'pg_') LOOP
  EXECUTE format('SELECT count(*) FROM %I.%I x WHERE x::text LIKE ANY($1) AND coalesce(to_jsonb(x)->>''id'','''')<>ALL($2)',r.nspname,r.relname)
   INTO hits USING ARRAY['%erasure-subject@example.invalid%','%Secret Person%','%Private street%','%Private city%'],
   ARRAY[current_setting('test.privacy_unowned_order'),current_setting('test.privacy_unowned_notification')];
  IF hits>0 THEN found:=found||format(' %s.%s=%s',r.nspname,r.relname,hits); END IF;
 END LOOP;
 IF found<>'' THEN RAISE EXCEPTION 'PRIVACY_GATE leftover PII:%',found; END IF;
END $no_leftover_pii$;
SELECT set_config('app.outbox_worker',current_setting('test.privacy_send_worker'),true),set_config('app.outbox_attempt','1',true);
SET LOCAL ROLE app_outbox_worker;
DO $notification_fence$
BEGIN
 IF app.notification_finish(current_setting('app.tenant_id')::uuid,current_setting('test.privacy_notification')::uuid,current_setting('test.privacy_send_token')::uuid,'sent',NULL) THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION stale finish'; END IF;
 BEGIN PERFORM app.notification_begin(current_setting('app.tenant_id')::uuid,current_setting('test.privacy_dispatch')::uuid,current_setting('test.privacy_notification')::uuid); RAISE EXCEPTION 'PRIVACY_NOTIFICATION stale begin'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $notification_fence$;
RESET ROLE;
-- Check expired capability independently from request expiry and single-use.
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','expiry@example.invalid') RETURNING id AS expiry_customer \gset
SELECT uuidv7() AS expiry_request \gset
SET LOCAL ROLE app_runtime;
SELECT app.data_rights_request_export(:'expiry_request',:'expiry_customer','service',uuidv7());
SELECT app.data_rights_complete_export(:'expiry_request',sha256('expiry-token'::bytea),sha256('expiry-content'::bytea),app.data_rights_download_deadline(:'expiry_request'));
RESET ROLE;
UPDATE app.data_rights_exports SET download_expires_at=clock_timestamp()-interval '1 second' WHERE id=:'expiry_request';
SELECT set_config('test.privacy_expiry_request',:'expiry_request',true),set_config('test.privacy_expiry_customer',:'expiry_customer',true);
SET LOCAL ROLE app_runtime;
DO $expiry$
BEGIN
 BEGIN PERFORM app.data_rights_consume_export(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid,sha256('expiry-token'::bytea)); RAISE EXCEPTION 'PRIVACY_GATE expired capability'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN PERFORM app.data_rights_status(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid); RAISE EXCEPTION 'PRIVACY_GATE missing tenant'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','not-a-uuid',true);
 BEGIN PERFORM app.data_rights_status(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid); RAISE EXCEPTION 'PRIVACY_GATE malformed tenant'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $expiry$;
RESET ROLE;
SELECT set_config('app.tenant_id',:'t',true);
DO $authority$
BEGIN
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_data_rights_executor' AND (rolcanlogin OR rolbypassrls OR rolsuper))
 OR EXISTS(SELECT FROM pg_auth_members WHERE roleid='app_data_rights_executor'::regrole OR member='app_data_rights_executor'::regrole) THEN RAISE EXCEPTION 'PRIVACY_GATE executor reachable'; END IF;
 IF NOT (SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='app.data_rights_exports'::regclass) THEN RAISE EXCEPTION 'PRIVACY_GATE forced RLS'; END IF;
 IF EXISTS(SELECT FROM app.customer_credentials WHERE customer_id=current_setting('test.privacy_customer')::uuid) THEN RAISE EXCEPTION 'PRIVACY_GATE credential residue'; END IF;
 IF NOT EXISTS(SELECT FROM app.site_releases WHERE id=current_setting('test.privacy_release')::uuid AND sealed) OR NOT EXISTS(SELECT FROM app.media_assets WHERE id=current_setting('test.privacy_media')::uuid AND visibility='public') OR NOT EXISTS(SELECT FROM app.media_blobs WHERE id=current_setting('test.privacy_blob')::uuid AND status='ready') THEN RAISE EXCEPTION 'PRIVACY_GATE unowned sealed media changed'; END IF;
 BEGIN UPDATE app.customers SET status='active' WHERE id=current_setting('test.privacy_customer')::uuid; RAISE EXCEPTION 'PRIVACY_GATE customer resurrection'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN INSERT INTO app.customer_credentials VALUES(current_setting('app.tenant_id')::uuid,current_setting('test.privacy_customer')::uuid,'$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA',now()); RAISE EXCEPTION 'PRIVACY_GATE credential resurrection'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Current schema has no customer sessions or customer-owned media binding:
 -- erasure never maps customer UUIDs to staff users or media_uploads.actor_id.
END $authority$;
-- Missing/foreign parents retain the original named composite FK (23503).
-- Only matched disabled/erased parents trigger the privacy lifecycle error (55000).
DO $parent_guards$
DECLARE t uuid:=current_setting('app.tenant_id')::uuid; parent uuid; guard text;
 phc text:='$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA';
 sku uuid:=current_setting('test.privacy_sku')::uuid; oid uuid; disabled uuid; r app.inventory_reservations; lines jsonb;
BEGIN
 FOREACH parent IN ARRAY ARRAY[current_setting('test.privacy_foreign')::uuid,uuidv7()] LOOP
  BEGIN
   INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash) VALUES(t,parent,phc);
   RAISE EXCEPTION 'PRIVACY_GATE missing credential parent accepted';
  EXCEPTION WHEN foreign_key_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard<>'customer_credentials_customer_fk' THEN RAISE EXCEPTION 'PRIVACY_GATE credential parent wrong FK %',guard; END IF;
  END;
  BEGIN
   INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code) VALUES(t,parent,'Fixture','Street','City','US');
   RAISE EXCEPTION 'PRIVACY_GATE missing address parent accepted';
  EXCEPTION WHEN foreign_key_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard<>'customer_addresses_customer_fk' THEN RAISE EXCEPTION 'PRIVACY_GATE address parent wrong FK %',guard; END IF;
  END;
  BEGIN
   oid:=uuidv7(); r:=app.inventory_reserve(sku,1,'order',oid,'privacy-missing-parent-'||oid::text,900);
   lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','frozen-sku','quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',r.id,'tax_snapshot','{}'::jsonb));
   PERFORM app.orders_create(oid,'privacy-missing-parent-'||oid::text,sha256('fixture'::bytea),parent,NULL,'USD',0,'{"amount_minor":0,"currency":"USD"}',lines,'[]');
   RAISE EXCEPTION 'PRIVACY_GATE missing order parent accepted';
  EXCEPTION WHEN foreign_key_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard<>'orders_customer_fk' THEN RAISE EXCEPTION 'PRIVACY_GATE order parent wrong FK %',guard; END IF;
  END;
 END LOOP;
 INSERT INTO app.customers(tenant_id,email,status) VALUES(t,'disabled-no-journal@example.invalid','disabled') RETURNING id INTO disabled;
 FOREACH parent IN ARRAY ARRAY[current_setting('test.privacy_customer')::uuid,disabled] LOOP
 BEGIN
  INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash) VALUES(t,parent,phc);
  RAISE EXCEPTION 'PRIVACY_GATE disabled credential parent accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'PRIVACY_CUSTOMER_DISABLED' THEN RAISE; END IF; END;
 BEGIN
  INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code) VALUES(t,parent,'Fixture','Street','City','US');
  RAISE EXCEPTION 'PRIVACY_GATE erased address parent accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'PRIVACY_CUSTOMER_DISABLED' THEN RAISE; END IF; END;
 BEGIN
  oid:=uuidv7(); r:=app.inventory_reserve(sku,1,'order',oid,'privacy-erased-parent-'||oid::text,900);
  lines:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','frozen-sku','quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,'reservation_id',r.id,'tax_snapshot','{}'::jsonb));
  PERFORM app.orders_create(oid,'privacy-erased-parent-'||oid::text,sha256('fixture'::bytea),parent,NULL,'USD',0,'{"amount_minor":0,"currency":"USD"}',lines,'[]');
  RAISE EXCEPTION 'PRIVACY_GATE erased order parent accepted';
 EXCEPTION WHEN object_not_in_prerequisite_state THEN
  IF SQLERRM<>'PRIVACY_CUSTOMER_DISABLED' THEN RAISE; END IF; END;
 END LOOP;
END $parent_guards$;
ROLLBACK;
