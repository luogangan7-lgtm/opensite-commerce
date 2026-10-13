-- Responsibility: quote-owned durable conversion and immutable snapshot gates.
-- Dependencies: quotes/orders controlled commands on expendable PostgreSQL18.
-- Caller: check_pg runner; all fixture writes roll back, runtime never writes tables.
-- Invariants: one order per quote, DB-clock approval/authority, atomic binding/outbox.
\set ON_ERROR_STOP on
DO $schema$ BEGIN
 IF to_regclass('app.quote_conversions') IS NULL THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_SCHEMA_REQUIRED';
 END IF;
END $schema$;
BEGIN;
INSERT INTO app.tenants(name) VALUES('quote-conversion-A') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('quote-conversion-B') RETURNING id AS foreign_t \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'t','conversion') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'t',:'org','conversion') RETURNING id AS site \gset
INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@example.invalid','fixture') RETURNING id AS staff \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'t',:'staff','admin');
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@example.invalid') RETURNING id AS buyer \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@example.invalid') RETURNING id AS approver \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'t',uuidv7()::text||'@example.invalid') RETURNING id AS outsider \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'t','conversion-company') RETURNING id AS company \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'t','other-company') RETURNING id AS other_company \gset
INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
 VALUES(:'t',:'company',:'buyer','purchaser'),(:'t',:'company',:'approver','approver'),(:'t',:'other_company',:'outsider','purchaser');
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message,buyer_company_id)
 VALUES(:'t',:'site','fixture@example.invalid','conversion',:'company') RETURNING id AS inquiry \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','conversion-product') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'product','conversion-sku','default') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('qc.t',:'t',true),set_config('qc.foreign_t',:'foreign_t',true),
 set_config('qc.site',:'site',true),set_config('qc.inquiry',:'inquiry',true),set_config('qc.company',:'company',true),
 set_config('qc.other_company',:'other_company',true),set_config('qc.staff',:'staff',true),set_config('qc.buyer',:'buyer',true),
 set_config('qc.approver',:'approver',true),set_config('qc.outsider',:'outsider',true),set_config('qc.product',:'product',true),set_config('qc.sku',:'sku',true);
CREATE FUNCTION pg_temp.qc_refuse(command text,wanted text,label text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN
 BEGIN EXECUTE command; EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=wanted THEN RETURN; END IF;
  RAISE EXCEPTION 'QUOTE_CONVERSION_WRONG_REFUSAL % expected % got % (%)',label,wanted,SQLSTATE,SQLERRM;
 END;
 RAISE EXCEPTION 'QUOTE_CONVERSION_EXPECTED_REFUSAL %',label;
END $$;
CREATE FUNCTION pg_temp.qc_quote() RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE t uuid:=current_setting('qc.t')::uuid; q uuid; BEGIN
 q:=app.create_quote(t,current_setting('qc.site')::uuid,current_setting('qc.inquiry')::uuid,
 current_setting('qc.company')::uuid,current_setting('qc.staff')::uuid,'USD',100,10,5,115,
 '{"shipping":{"ZoneID":"zone","RateID":"rate","Method":"fixed","Charge":{"Amount":5,"Currency":"USD"}}}',clock_timestamp()+interval '1 hour',
 jsonb_build_array(jsonb_build_object('line_no',1,'product_id',current_setting('qc.product')::uuid,
 'variant_id',current_setting('qc.sku')::uuid,'description','negotiated','quantity',2,'unit_price',50,'line_total',100,
 'snapshot',jsonb_build_object('id','1','amounts',jsonb_build_object('net',100,'tax',10,'gross',110),'snapshot',jsonb_build_object('currency','USD','reason','fixture')))));
 PERFORM app.transition_quote(t,q,1,'send','staff',current_setting('qc.staff')::uuid);
 PERFORM app.transition_quote(t,q,2,'accept','customer',current_setting('qc.buyer')::uuid);
 PERFORM app.transition_quote(t,q,3,'approve','customer',current_setting('qc.approver')::uuid);
 RETURN q;
END $$;
CREATE FUNCTION pg_temp.qc_order(p_unit bigint DEFAULT 50,p_net bigint DEFAULT 100,p_tax bigint DEFAULT 10) RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE oid uuid:=uuidv7(); r app.inventory_reservations; o app.orders; BEGIN
 r:=app.inventory_reserve(current_setting('qc.sku')::uuid,2,'order',oid,'qc-reserve:'||oid,900);
 o:=app.orders_create(oid,'qc-create:'||oid,sha256(oid::text::bytea),current_setting('qc.buyer')::uuid,NULL,'USD',5,
 '{"ZoneID":"zone","RateID":"rate","Method":"fixed","Charge":{"Amount":5,"Currency":"USD"}}',
 jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',current_setting('qc.sku')::uuid,'sku','conversion-sku','quantity',2,
 'unit_price_minor',p_unit,'line_net_minor',p_net,'line_tax_minor',p_tax,'line_total_minor',p_net+p_tax,'reservation_id',r.id,
 'tax_snapshot',jsonb_build_object('currency','USD','reason','fixture'))),'[]'); RETURN o.id;
END $$;
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',100,'conversion fixture',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
DO $conversion$
DECLARE t uuid:=current_setting('qc.t')::uuid; company uuid:=current_setting('qc.company')::uuid;
 buyer uuid:=current_setting('qc.buyer')::uuid; q uuid; q2 uuid; oid uuid; bad uuid; c app.quote_conversions; qr app.quotes; BEGIN
 q:=pg_temp.qc_quote(); q2:=pg_temp.qc_quote(); PERFORM set_config('qc.q',q::text,true); PERFORM set_config('qc.q2',q2::text,true);
 qr:=app.quote_conversion_lock(t,q,company,buyer);
 IF qr.id<>q OR qr.status<>'approved' THEN RAISE EXCEPTION 'QUOTE_CONVERSION_LOCK_SNAPSHOT'; END IF;
 -- Optional receipt lookup must not abort the same transaction before create.
 c:=app.quote_conversion_get(t,q,company,'customer',buyer);
 IF c IS NOT NULL OR EXISTS(SELECT FROM app.quote_conversion_get(t,q,company,'customer',buyer) receipt
 JOIN app.orders existing ON(existing.tenant_id,existing.id)=(receipt.tenant_id,receipt.order_id)) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_ABSENT_RECEIPT'; END IF;
 PERFORM 1; -- A raised P0002 here would abort before subsequent order commands.
 oid:=pg_temp.qc_order(); PERFORM set_config('qc.order',oid::text,true);
 c:=app.quote_conversion_bind(t,q,company,buyer,oid,4,'conversion-key',sha256('conversion'::bytea));
 IF c.order_id<>oid OR c.quote_id<>q THEN RAISE EXCEPTION 'QUOTE_CONVERSION_BIND_RESULT'; END IF;
 c:=app.quote_conversion_bind(t,q,company,buyer,oid,4,'conversion-key',sha256('conversion'::bytea));
 IF (SELECT count(*) FROM app.quote_conversions)<>1
 OR (SELECT count(*) FROM app.outbox_events WHERE event_type='quote.converted')<>1 THEN RAISE EXCEPTION 'QUOTE_CONVERSION_REPLAY_DUPLICATE'; END IF;
 -- Conversion and replay must persist one notification for the receipt owner.
 -- The inquiry contact and approval actor are different fixture recipients;
 -- lifecycle payloads carry references, never recipient authority.
 IF (SELECT count(*) FROM app.notification_deliveries WHERE tenant_id=t AND template_key='quote.converted')<>1 THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOTIFICATION_REQUIRED'; END IF;
 IF NOT EXISTS(SELECT FROM app.notification_deliveries d
  JOIN app.quote_conversions receipt ON(receipt.tenant_id,receipt.quote_id)=(d.tenant_id,d.reference_id)
  JOIN app.customers owner ON(owner.tenant_id,owner.id)=(receipt.tenant_id,receipt.customer_id)
  JOIN app.outbox_events source ON(source.tenant_id,source.id)=(d.tenant_id,d.source_event_id)
  JOIN app.outbox_events dispatch ON(dispatch.tenant_id,dispatch.id)=(d.tenant_id,d.dispatch_event_id)
  WHERE d.tenant_id=t AND d.template_key='quote.converted' AND d.reference_id=q
   AND d.recipient=owner.email_normalized AND owner.id=buyer AND d.status='pending'
   AND source.event_type='quote.converted' AND source.delivery_target='quotes'
   AND source.payload->>'quote_id'=receipt.quote_id::text AND source.payload->>'order_id'=receipt.order_id::text
   AND source.payload->>'quote_version'=receipt.quote_version::text
   AND dispatch.event_type='notification.dispatch' AND dispatch.delivery_target='notifications.email'
   AND dispatch.payload=jsonb_build_object('notification_id',d.id)) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_NOTIFICATION_AUTHORITY'; END IF;
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''conversion-key'',sha256(''changed''::bytea))',t,q,company,buyer,oid),'22000','hash reuse');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''different-key'',sha256(''conversion''::bytea))',t,q,company,buyer,oid),'55000','same quote different key');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''conversion-key'',sha256(''conversion''::bytea))',t,q2,company,buyer,oid),'22000','same key other quote');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''second-key'',sha256(''conversion''::bytea))',t,q2,company,buyer,oid),'23505','same order second quote');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_lock(%L,%L,%L,%L)',t,q,current_setting('qc.other_company'),buyer),'P0002','stamped wrong company');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_lock(%L,%L,%L,%L)',t,q,company,current_setting('qc.outsider')),'P0002','outsider');
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_lock(%L,%L,%L,%L)',current_setting('qc.foreign_t'),q,company,buyer),'P0002','foreign tenant');
 PERFORM pg_temp.qc_refuse('UPDATE app.quote_conversions SET quote_version=99','42501','runtime direct update');
 PERFORM pg_temp.qc_refuse('DELETE FROM app.quote_conversions','42501','runtime direct delete');
 PERFORM pg_temp.qc_refuse(format('INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key) VALUES(%L,''quote.converted'',''{}'',''quotes'',''converted:forged'')',t),'42501','forged converted event');
 PERFORM app.orders_validate_payment_reservations(oid);
 -- Same total cannot hide a changed tax/net/price composition.
 bad:=pg_temp.qc_order(49,98,12);
 PERFORM pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''bad-key'',sha256(''bad''::bytea))',t,q2,company,buyer,bad),'23514','same total different snapshot');
 IF EXISTS(SELECT FROM app.quote_conversions WHERE quote_id=q2) THEN RAISE EXCEPTION 'QUOTE_CONVERSION_BAD_SNAPSHOT_BOUND'; END IF;
END $conversion$;
RESET ROLE;
-- Failing the event INSERT rolls the binding statement back as well.
SET LOCAL ROLE app_runtime;
SELECT pg_temp.qc_order() AS good_order \gset
RESET ROLE;
REVOKE INSERT(tenant_id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events FROM app_quotes_executor;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''outbox-key'',sha256(''outbox''::bytea))',:'t',current_setting('qc.q2'),:'company',:'buyer',:'good_order'),'42501','outbox atomic rollback');
RESET ROLE;
GRANT INSERT(tenant_id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events TO app_quotes_executor;
DO $atomic$ BEGIN
 IF EXISTS(SELECT FROM app.quote_conversions WHERE quote_id=current_setting('qc.q2')::uuid) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_FAILED_OUTBOX_BOUND'; END IF;
END $atomic$;
-- A dispatch failure must roll receipt, source and delivery back together.
REVOKE INSERT(tenant_id,id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events FROM app_notifications_executor;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.qc_refuse(format('SELECT app.quote_conversion_bind(%L,%L,%L,%L,%L,4,''dispatch-key'',sha256(''dispatch''::bytea))',:'t',current_setting('qc.q2'),:'company',:'buyer',:'good_order'),'42501','notification dispatch atomic rollback');
RESET ROLE;
GRANT INSERT(tenant_id,id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events TO app_notifications_executor;
DO $notification_atomic$ BEGIN
 IF EXISTS(SELECT FROM app.quote_conversions WHERE quote_id=current_setting('qc.q2')::uuid)
 OR EXISTS(SELECT FROM app.outbox_events WHERE event_type='quote.converted' AND payload->>'quote_id'=current_setting('qc.q2'))
 OR EXISTS(SELECT FROM app.notification_deliveries WHERE template_key='quote.converted' AND reference_id=current_setting('qc.q2')::uuid) THEN
  RAISE EXCEPTION 'QUOTE_CONVERSION_FAILED_NOTIFICATION_BOUND'; END IF;
END $notification_atomic$;
-- Preflight must decide DB expiry before a caller invokes Capture; cancellation
-- still releases an expired active hold without producing a paid event.
UPDATE app.inventory_reservations SET expires_at=clock_timestamp()-interval '1 second'
 WHERE owner_id=current_setting('qc.order')::uuid;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.qc_refuse(format('SELECT app.orders_validate_payment_reservations(%L)',current_setting('qc.order')),'55000','expired payment preflight');
SELECT app.orders_transition(current_setting('qc.order')::uuid,'cancelled','fixture-cancel',0);
DO $cancel$ BEGIN
 IF EXISTS(SELECT FROM app.inventory_reservations WHERE owner_id=current_setting('qc.order')::uuid AND state<>'released')
 OR EXISTS(SELECT FROM app.outbox_events WHERE event_type='order.paid')
 OR EXISTS(SELECT FROM app.ledger_transactions) THEN RAISE EXCEPTION 'QUOTE_CONVERSION_FAILED_PAYMENT_EFFECT'; END IF;
END $cancel$;
RESET ROLE;
-- Fixture deliberately expires a persisted row; production cannot mutate it.
ALTER TABLE app.quotes DISABLE TRIGGER quote_guard;
UPDATE app.quotes SET expires_at=clock_timestamp()-interval '1 second' WHERE id IN(current_setting('qc.q')::uuid,current_setting('qc.q2')::uuid);
ALTER TABLE app.quotes ENABLE TRIGGER quote_guard;
SET LOCAL ROLE app_runtime;
SELECT app.quote_conversion_get(:'t',current_setting('qc.q')::uuid,:'company','customer',:'buyer');
SELECT app.quote_conversion_lock(:'t',current_setting('qc.q')::uuid,:'company',:'buyer');
SELECT pg_temp.qc_refuse(format('SELECT app.quote_conversion_lock(%L,%L,%L,%L)',:'t',current_setting('qc.q2'),:'company',:'buyer'),'22023','expired new conversion');
RESET ROLE;
UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=:'t' AND customer_id=:'buyer';
SET LOCAL ROLE app_runtime;
SELECT pg_temp.qc_refuse(format('SELECT app.quote_conversion_get(%L,%L,%L,''customer'',%L)',:'t',current_setting('qc.q'),:'company',:'buyer'),'P0002','revoked history access');
RESET ROLE;
DO $immutable$ BEGIN
 PERFORM pg_temp.qc_refuse('UPDATE app.quote_conversions SET request_hash=sha256(''changed''::bytea)','55000','owner immutability');
END $immutable$;
ROLLBACK;
SELECT 'PASS: quote conversion unique binding, actor/DB expiry, snapshots, history and immutable receipt' AS result;
