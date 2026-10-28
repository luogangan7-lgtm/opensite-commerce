-- stable-id: b-privacy-hardening
-- depends-on: c22a-data-rights
-- depends-on: c18b-quote-conversion
-- Responsibility: bounded privacy exports, committed terminal outcomes and trusted erasure ownership.
-- Dependencies: data-rights commands and immutable quote conversion receipts; called by existing runtime queries.
-- Invariants: one export snapshot, unchanged quotas, no contact-derived authority, no new runtime table access.
-- Compatibility: forward-only; revoked export rows remain the durable filesystem Sweep worklist.
-- Rollback: revert application changes; retain these additive ownership receipts and stricter guards.
ALTER TABLE app.data_rights_exports DROP CONSTRAINT data_rights_exports_status_check;
ALTER TABLE app.data_rights_exports ADD CONSTRAINT data_rights_exports_status_check
 CHECK(status IN('pending','ready','consumed','revoked','failed','expired'));
ALTER TABLE app.data_rights_exports DROP CONSTRAINT data_rights_exports_check;
ALTER TABLE app.data_rights_exports ADD CONSTRAINT data_rights_exports_check CHECK(
 (status='pending' AND token_hash IS NULL AND content_hash IS NULL)
 OR status='revoked'
 OR (status IN('failed','expired') AND token_hash IS NULL AND content_hash IS NULL AND download_expires_at IS NULL)
 OR (status IN('ready','consumed') AND token_hash IS NOT NULL AND content_hash IS NOT NULL
  AND download_expires_at IS NOT NULL AND download_expires_at<=expires_at));
-- A mailbox proof is deleted on erasure; only this typed, append-only receipt
-- preserves its order ownership. Neither guest JSON nor a matching email can.
CREATE TABLE app.data_rights_erased_orders(
 tenant_id uuid NOT NULL,order_id uuid NOT NULL,customer_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,order_id),
 FOREIGN KEY(tenant_id,order_id) REFERENCES app.orders(tenant_id,id),
 FOREIGN KEY(tenant_id,customer_id) REFERENCES app.data_rights_erasures(tenant_id,customer_id) DEFERRABLE INITIALLY DEFERRED
);
ALTER TABLE app.data_rights_erased_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.data_rights_erased_orders FORCE ROW LEVEL SECURITY;
REVOKE ALL ON app.data_rights_erased_orders FROM PUBLIC;
DO $acl$ DECLARE r text; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  EXECUTE format('REVOKE ALL ON app.data_rights_erased_orders FROM %I',r);
 END LOOP;
END $acl$;
CREATE POLICY tenant_scope ON app.data_rights_erased_orders TO app_data_rights_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT,INSERT ON app.data_rights_erased_orders TO app_data_rights_executor;
CREATE TRIGGER data_rights_erased_orders_immutable BEFORE UPDATE OR DELETE ON app.data_rights_erased_orders
 FOR EACH ROW EXECUTE FUNCTION app.data_rights_erasure_guard();
-- Registered ownership can be recovered only from typed IDs. Legacy guest
-- erasures already deleted their proof link; never guess its customer from JSON.
INSERT INTO app.data_rights_erased_orders(tenant_id,order_id,customer_id)
 SELECT o.tenant_id,o.id,e.customer_id FROM app.orders o JOIN app.data_rights_erasures e
 ON(e.tenant_id,e.customer_id)=(o.tenant_id,o.customer_id)
 UNION SELECT l.tenant_id,l.order_id,l.customer_id FROM app.customer_guest_order_links l
 JOIN app.data_rights_erasures e ON(e.tenant_id,e.customer_id)=(l.tenant_id,l.customer_id);
-- Reviewed legacy repair may restore independently proven mailbox links before
-- retrying this migration. Capture the typed receipt, then retire that proof.
DELETE FROM app.customer_guest_order_links l USING app.data_rights_erased_orders e
 WHERE(e.tenant_id,e.order_id,e.customer_id)=(l.tenant_id,l.order_id,l.customer_id);
DO $legacy_guest$ BEGIN
 IF EXISTS(SELECT FROM app.orders o WHERE o.customer_id IS NULL
  AND o.shipping_snapshot=jsonb_build_object('erased',true,'amount_minor',o.shipping_minor,'currency',o.currency)
  AND o.guest_contact=jsonb_build_object('email','erased-'||o.id::text||'@invalid.test','erased',true)
  AND NOT EXISTS(SELECT FROM app.data_rights_erased_orders e WHERE(e.tenant_id,e.order_id)=(o.tenant_id,o.id))) THEN
  RAISE EXCEPTION 'PRIVACY_LEGACY_GUEST_REVIEW_REQUIRED' USING ERRCODE='55000';
 END IF;
END $legacy_guest$;
GRANT SELECT(tenant_id,quote_id,customer_id) ON app.quote_conversions TO app_data_rights_executor;
CREATE POLICY data_rights_scope ON app.quote_conversions TO app_data_rights_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE OR REPLACE FUNCTION app.data_rights_status(p_id uuid,p_customer uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();r app.data_rights_exports;
BEGIN
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id AND customer_id=p_customer;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 RETURN (to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'])||jsonb_build_object(
  'token_hash',encode(r.token_hash,'hex'),'download_available',r.status='ready'
   AND r.expires_at>clock_timestamp() AND r.download_expires_at>clock_timestamp()
   AND EXISTS(SELECT FROM app.customers WHERE tenant_id=t AND id=p_customer AND status='active')
   AND NOT EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=p_customer));
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_lock_export(p_id uuid) RETURNS app.data_rights_exports LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();r app.data_rights_exports;c app.customers;
BEGIN
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=r.customer_id FOR UPDATE;
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF c.status<>'active' OR r.status IN('consumed','revoked','failed','expired') OR r.expires_at<=clock_timestamp()
  OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=c.id) THEN RAISE EXCEPTION 'PRIVACY_REVOKED_EXPIRED' USING ERRCODE='55000'; END IF;
 RETURN r;
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_worker_info(p_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();r app.data_rights_exports;c app.customers;
BEGIN
 -- Same artifact -> customer -> request order as construction and Sweep.
 PERFORM app.data_rights_artifact_live(p_id);
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=r.customer_id FOR UPDATE;
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF r.status IN('pending','ready') THEN
  IF c.status<>'active' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=r.customer_id) THEN
   UPDATE app.data_rights_exports SET status='revoked',token_hash=NULL,content_hash=NULL,download_expires_at=NULL WHERE tenant_id=t AND id=p_id RETURNING * INTO r;
  ELSIF r.expires_at<=clock_timestamp() OR (r.status='ready' AND r.download_expires_at<=clock_timestamp()) THEN
   UPDATE app.data_rights_exports SET status='expired',token_hash=NULL,content_hash=NULL,download_expires_at=NULL WHERE tenant_id=t AND id=p_id RETURNING * INTO r;
  END IF;
 END IF;
 RETURN (to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'])||jsonb_build_object('content_hash',encode(r.content_hash,'hex'));
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_build_export(p_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id); j jsonb; n bigint;
BEGIN
 IF r.status<>'pending' THEN RAISE EXCEPTION 'PRIVACY_NOT_PENDING' USING ERRCODE='55000'; END IF;
 BEGIN
 WITH orders AS MATERIALIZED(SELECT o.* FROM app.orders o WHERE o.tenant_id=r.tenant_id
  AND o.id IN(SELECT app.data_rights_customer_orders(r.tenant_id,r.customer_id)) ORDER BY o.id LIMIT 10001),
 addresses AS MATERIALIZED(SELECT a.* FROM app.customer_addresses a WHERE a.tenant_id=r.tenant_id AND a.customer_id=r.customer_id ORDER BY a.id LIMIT 10001),
 lines AS MATERIALIZED(SELECT l.* FROM app.order_lines l WHERE l.tenant_id=r.tenant_id AND l.order_id IN(SELECT id FROM orders) ORDER BY l.id LIMIT 10001),
 carts AS MATERIALIZED(SELECT c.* FROM app.carts c WHERE c.tenant_id=r.tenant_id AND c.customer_id=r.customer_id ORDER BY c.id LIMIT 10001),
 cart_lines AS MATERIALIZED(SELECT l.* FROM app.cart_lines l WHERE l.tenant_id=r.tenant_id AND l.cart_id IN(SELECT id FROM carts) ORDER BY l.cart_id,l.sku_id LIMIT 10001),
 counts AS MATERIALIZED(SELECT (SELECT count(*) FROM addresses)+(SELECT count(*) FROM orders)
  +(SELECT count(*) FROM lines)+(SELECT count(*) FROM carts)+(SELECT count(*) FROM cart_lines) AS n)
 SELECT CASE WHEN counts.n<=10000 THEN jsonb_build_object('schema_version',1,'customer',jsonb_build_object('id',c.id,'email',c.email,'display_name',c.display_name,'status',c.status,'created_at',c.created_at),
 'addresses',coalesce((SELECT jsonb_agg(to_jsonb(a)-ARRAY['tenant_id','customer_id'] ORDER BY a.id) FROM addresses a),'[]'::jsonb),
 'orders',coalesce((SELECT jsonb_agg(jsonb_build_object('id',o.id,'order_number',o.order_number,'currency',o.currency,'total_minor',o.total_minor,'state',o.state,'guest_contact',o.guest_contact,'shipping_snapshot',o.shipping_snapshot,'created_at',o.created_at,
 'lines',coalesce((SELECT jsonb_agg(jsonb_build_object('sku',l.sku,'quantity',l.quantity,'unit_price_minor',l.unit_price_minor,'line_net_minor',l.line_net_minor,'line_tax_minor',l.line_tax_minor,'line_total_minor',l.line_total_minor,'tax_snapshot',l.tax_snapshot) ORDER BY l.id) FROM lines l WHERE l.order_id=o.id),'[]'::jsonb)) ORDER BY o.id) FROM orders o),'[]'::jsonb),
 'carts',coalesce((SELECT jsonb_agg(jsonb_build_object('id',ca.id,'currency',ca.currency,'lines',coalesce((SELECT jsonb_agg(jsonb_build_object('sku_id',l.sku_id,'quantity',l.quantity) ORDER BY l.sku_id) FROM cart_lines l WHERE l.cart_id=ca.id),'[]'::jsonb)) ORDER BY ca.id) FROM carts ca),'[]'::jsonb)) END,counts.n INTO j,n
 FROM counts CROSS JOIN app.customers c WHERE (c.tenant_id,c.id)=(r.tenant_id,r.customer_id);
 IF n>10000 THEN RAISE EXCEPTION 'PRIVACY_EXPORT_RECORD_LIMIT' USING ERRCODE='22023'; END IF;
 IF octet_length(j::text)>10485760 THEN RAISE EXCEPTION 'PRIVACY_EXPORT_BYTES_LIMIT' USING ERRCODE='22023'; END IF;
 RETURN j;
EXCEPTION WHEN invalid_parameter_value THEN
 -- Commit a terminal state through the existing query. Other 22023 errors
 -- remain errors; a function exception must never erase failed state by rollback.
 IF SQLERRM NOT IN('PRIVACY_EXPORT_RECORD_LIMIT','PRIVACY_EXPORT_BYTES_LIMIT') THEN RAISE; END IF;
 UPDATE app.data_rights_exports SET status='failed',token_hash=NULL,content_hash=NULL,download_expires_at=NULL
  WHERE tenant_id=r.tenant_id AND id=r.id;
 RETURN jsonb_build_object('status','failed');
 END;
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_erase_customer(p_customer uuid,p_actor_kind text,p_actor_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();c app.customers; owned uuid[]; requests jsonb; naddr integer;ncred integer;nb2b integer;norder integer;nline integer;nnotification integer;ncheckout integer;nattempt integer;nquote integer;nquoteline integer;nadjust integer;nreceipt integer;nauth integer;nshipment integer;nreturn integer;nmovement integer;n integer;
BEGIN
 IF p_customer IS NULL OR p_actor_id IS NULL OR p_actor_kind IS NULL OR p_actor_kind NOT IN('user','api_key','service') THEN RAISE EXCEPTION 'PRIVACY_INVALID' USING ERRCODE='22023'; END IF;
 -- Existing financial writers lock order before notification fanout locks customer.
 PERFORM id FROM app.orders WHERE tenant_id=t AND id IN(SELECT app.data_rights_customer_orders(t,p_customer)) ORDER BY id FOR UPDATE;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=p_customer FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 -- Catch orders committed while the first pass waited for customer admission.
 -- Fail atomically rather than wait on a transition that now needs our customer.
 PERFORM id FROM app.orders WHERE tenant_id=t AND id IN(SELECT app.data_rights_customer_orders(t,p_customer)) ORDER BY id FOR UPDATE NOWAIT;
 -- Fixed before any link is deleted: own + mailbox-linked guest orders.
 SELECT coalesce(array_agg(o ORDER BY o),'{}') INTO owned FROM app.data_rights_customer_orders(t,p_customer) o;
 INSERT INTO app.data_rights_erased_orders(tenant_id,order_id,customer_id)
  SELECT t,o,p_customer FROM unnest(owned) o ON CONFLICT DO NOTHING;
 -- Fulfillment writers lock Order -> Returns -> Inventory; the Order fence
 -- serializes them with erasure even while PSP IO occurs outside the DB.
 -- NOWAIT on copies prevents inversion with an older/in-flight writer.
 PERFORM id FROM app.returns WHERE tenant_id=t AND order_id=ANY(owned) ORDER BY id FOR UPDATE NOWAIT;
 PERFORM id FROM app.shipments WHERE tenant_id=t AND order_id=ANY(owned) ORDER BY id FOR UPDATE NOWAIT;
 PERFORM m.id FROM app.inventory_movements m JOIN app.returns r ON(r.tenant_id,r.id)=(m.tenant_id,m.owner_id)
  WHERE m.tenant_id=t AND m.owner_type='return' AND r.order_id=ANY(owned) ORDER BY m.id FOR UPDATE OF m NOWAIT;
 UPDATE app.shipments SET carrier='[erased]',tracking_number='[erased]' WHERE tenant_id=t AND order_id=ANY(owned);GET DIAGNOSTICS nshipment=ROW_COUNT;
 UPDATE app.returns SET reason='[erased]',
  creation_response=CASE WHEN creation_response IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END,
  receive_response=CASE WHEN receive_response IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END
  WHERE tenant_id=t AND order_id=ANY(owned);GET DIAGNOSTICS nreturn=ROW_COUNT;
 UPDATE app.inventory_movements m SET reason='[erased]' FROM app.returns r
  WHERE (r.tenant_id,r.id)=(m.tenant_id,m.owner_id) AND m.tenant_id=t AND m.owner_type='return' AND r.order_id=ANY(owned);GET DIAGNOSTICS nmovement=ROW_COUNT;
 PERFORM id FROM app.data_rights_exports WHERE tenant_id=t AND customer_id=p_customer ORDER BY id FOR UPDATE;
 SELECT coalesce(jsonb_agg(id ORDER BY id),'[]'::jsonb) INTO requests FROM (SELECT id FROM app.data_rights_exports WHERE tenant_id=t AND customer_id=p_customer ORDER BY id LIMIT 100) cleanup;
 UPDATE app.data_rights_exports SET status='revoked',token_hash=NULL,content_hash=NULL WHERE tenant_id=t AND customer_id=p_customer;
 -- Lock all existing dispatch generations before their delivery rows, matching
 -- begin/finish. Payload binds a delivery ID; emails never establish ownership.
 PERFORM e.id FROM app.outbox_events e JOIN app.notification_deliveries d
  ON d.tenant_id=e.tenant_id AND e.payload=jsonb_build_object('notification_id',d.id)
  WHERE d.tenant_id=t AND app.data_rights_notification_owner(d.tenant_id,d.template_key,d.reference_id)=p_customer AND e.delivery_target='notifications.email'
  ORDER BY e.id FOR UPDATE OF e;
 PERFORM d.id FROM app.notification_deliveries d
  WHERE d.tenant_id=t AND app.data_rights_notification_owner(d.tenant_id,d.template_key,d.reference_id)=p_customer ORDER BY d.id FOR UPDATE OF d NOWAIT;
 -- A concurrent redrive can introduce a generation during the first pass.
 -- NOWAIT on delivery and this second dispatch pass prevents the inverse
 -- delivery -> newly admitted worker's outbox lock from becoming a deadlock.
 -- Contention aborts the entire erasure; callers may retry without partial PII.
 PERFORM e.id FROM app.outbox_events e JOIN app.notification_deliveries d
  ON d.tenant_id=e.tenant_id AND e.payload=jsonb_build_object('notification_id',d.id)
  WHERE d.tenant_id=t AND app.data_rights_notification_owner(d.tenant_id,d.template_key,d.reference_id)=p_customer AND e.delivery_target='notifications.email'
  ORDER BY e.id FOR UPDATE OF e NOWAIT;
 UPDATE app.outbox_events e SET status=CASE WHEN e.status='delivered' THEN 'delivered' ELSE 'dead_letter' END,lease_owner=NULL,lease_until=NULL,last_error='PRIVACY_ERASED'
  FROM app.notification_deliveries d WHERE d.tenant_id=e.tenant_id AND e.payload=jsonb_build_object('notification_id',d.id)
  AND d.tenant_id=t AND app.data_rights_notification_owner(d.tenant_id,d.template_key,d.reference_id)=p_customer AND e.delivery_target='notifications.email';
 UPDATE app.notification_deliveries d SET recipient='erased-'||d.id::text||'@invalid.test',status='dead_letter',last_error='PRIVACY_ERASED',send_token=NULL,lease_attempt=NULL,lease_worker=NULL,sent_at=NULL,updated_at=clock_timestamp()
  WHERE d.tenant_id=t AND app.data_rights_notification_owner(d.tenant_id,d.template_key,d.reference_id)=p_customer AND d.recipient<>'';GET DIAGNOSTICS nnotification=ROW_COUNT;

 -- Other declared copies (C-PII identity paths). Their writers lock the copy
 -- before the customer, so NOWAIT turns contention into a retryable busy
 -- rollback instead of a deadlock; nothing is partially erased.
 PERFORM s.id FROM app.checkout_sessions s WHERE s.tenant_id=t AND s.order_id =ANY(owned) ORDER BY s.id FOR UPDATE NOWAIT;
 PERFORM a.event_key FROM app.checkout_attempts a JOIN app.checkout_sessions s ON(s.tenant_id,s.id)=(a.tenant_id,a.checkout_id)
  WHERE s.tenant_id=t AND s.order_id =ANY(owned) ORDER BY a.event_kind,a.event_key FOR UPDATE OF a NOWAIT;
 PERFORM id FROM app.quotes WHERE tenant_id=t AND accepted_by=p_customer ORDER BY id FOR UPDATE NOWAIT;
 PERFORM operation FROM app.idempotency_records WHERE tenant_id=t AND app.data_rights_receipt_owned(p_customer,actor_kind,actor_id,operation,response_body) ORDER BY operation,idempotency_key FOR UPDATE NOWAIT;
 UPDATE app.checkout_attempts a SET result='{"erased":true}' FROM app.checkout_sessions s WHERE (s.tenant_id,s.id)=(a.tenant_id,a.checkout_id)
  AND s.tenant_id=t AND s.order_id =ANY(owned) AND a.result IS NOT NULL;GET DIAGNOSTICS nattempt=ROW_COUNT;
 UPDATE app.checkout_sessions SET address_snapshot='{"erased":true}',
  start_result=CASE WHEN start_result IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END,
  final_result=CASE WHEN final_result IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END
  WHERE tenant_id=t AND order_id =ANY(owned);GET DIAGNOSTICS ncheckout=ROW_COUNT;
 UPDATE app.quote_lines l SET description='[erased]',snapshot='{"erased":true}' FROM app.quotes q
  WHERE (q.tenant_id,q.id)=(l.tenant_id,l.quote_id) AND q.tenant_id=t AND q.accepted_by=p_customer;GET DIAGNOSTICS nquoteline=ROW_COUNT;
 UPDATE app.quotes SET snapshot=app.data_rights_quote_scrub(snapshot) WHERE tenant_id=t AND accepted_by=p_customer;GET DIAGNOSTICS nquote=ROW_COUNT;
 UPDATE app.order_adjustments SET reason='[erased]' WHERE tenant_id=t AND order_id =ANY(owned);GET DIAGNOSTICS nadjust=ROW_COUNT;
 UPDATE app.idempotency_records SET
  response_body=CASE WHEN response_body IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END,
  response_body_reference=CASE WHEN response_body_reference IS NULL THEN NULL ELSE 'erased' END
  WHERE tenant_id=t AND app.data_rights_receipt_owned(p_customer,actor_kind,actor_id,operation,response_body) AND (response_body IS NOT NULL OR response_body_reference IS NOT NULL);GET DIAGNOSTICS nreceipt=ROW_COUNT;
 -- Customer-auth artifacts: pending tokens (email copy, pending PHC), sessions
 -- and mailbox-proven guest links. Account admission counters are unowned.
 DELETE FROM app.customer_auth_tokens WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS nauth=ROW_COUNT;
 DELETE FROM app.customer_sessions WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS n=ROW_COUNT;nauth:=nauth+n;
 DELETE FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS n=ROW_COUNT;nauth:=nauth+n;
 DELETE FROM app.customer_addresses WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS naddr=ROW_COUNT;
 DELETE FROM app.customer_credentials WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS ncred=ROW_COUNT;
 UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=t AND customer_id=p_customer AND status<>'revoked';GET DIAGNOSTICS nb2b=ROW_COUNT;
 UPDATE app.orders SET shipping_snapshot=jsonb_build_object('erased',true,'amount_minor',shipping_minor,'currency',currency),
  guest_contact=CASE WHEN guest_contact IS NULL THEN NULL ELSE jsonb_build_object('email','erased-'||id::text||'@invalid.test','erased',true) END
  WHERE tenant_id=t AND id=ANY(owned);GET DIAGNOSTICS norder=ROW_COUNT;
 UPDATE app.order_lines l SET tax_snapshot=app.data_rights_tax_scrub(l.tax_snapshot) WHERE l.tenant_id=t AND l.order_id=ANY(owned);GET DIAGNOSTICS nline=ROW_COUNT;
 UPDATE app.customers SET email='erased+'||id::text||'@erased.invalid',display_name='[erased]',status='disabled' WHERE tenant_id=t AND id=p_customer;
 INSERT INTO app.data_rights_erasures(tenant_id,customer_id,actor_kind,actor_id) VALUES(t,p_customer,p_actor_kind,p_actor_id) ON CONFLICT DO NOTHING;
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,payload) VALUES(t,p_actor_kind,p_actor_id,'data_rights.customer.erase','customer',p_customer,jsonb_build_object('scope','customer','addresses',naddr,'credentials',ncred,'b2b',nb2b,'orders',norder,'order_lines',nline,'notifications',nnotification,'checkouts',ncheckout,'checkout_attempts',nattempt,'quotes',nquote,'quote_lines',nquoteline,'order_adjustments',nadjust,'command_receipts',nreceipt,'customer_auth',nauth,'shipments',nshipment,'returns',nreturn,'return_movements',nmovement));
 RETURN jsonb_build_object('customer_id',p_customer,'requests',requests,'counts',jsonb_build_object('addresses',naddr,'credentials',ncred,'b2b',nb2b,'orders',norder,'order_lines',nline,'notifications',nnotification,'checkouts',ncheckout,'checkout_attempts',nattempt,'quotes',nquote,'quote_lines',nquoteline,'order_adjustments',nadjust,'command_receipts',nreceipt,'customer_auth',nauth,'shipments',nshipment,'returns',nreturn,'return_movements',nmovement));
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_notification_owner(p_tenant uuid,p_template text,p_reference uuid) RETURNS uuid LANGUAGE sql STABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT CASE WHEN p_template IN('order.paid','order.shipped','order.refund') THEN coalesce((SELECT customer_id FROM app.orders WHERE tenant_id=p_tenant AND id=p_reference),
   (SELECT customer_id FROM app.customer_guest_order_links WHERE tenant_id=p_tenant AND order_id=p_reference))
  WHEN p_template='quote.converted' THEN (SELECT customer_id FROM app.quote_conversions WHERE tenant_id=p_tenant AND quote_id=p_reference)
  WHEN p_template='quote.approved' THEN (SELECT accepted_by FROM app.quotes WHERE tenant_id=p_tenant AND id=p_reference) END;
$fn$;

CREATE OR REPLACE FUNCTION app.data_rights_notification_insert_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $fn$
DECLARE customer uuid;c app.customers;erased boolean:=false;
BEGIN
 IF NEW.template_key NOT IN('order.paid','order.shipped','order.refund','quote.approved','quote.converted') THEN RETURN NEW; END IF;
 PERFORM set_config('app.tenant_id',NEW.tenant_id::text,true);
 -- A scrubbed (formerly linked) guest order stays erased after its link is gone.
 IF NEW.template_key IN('order.paid','order.shipped','order.refund') THEN
  erased:=EXISTS(SELECT FROM app.data_rights_erased_orders WHERE tenant_id=NEW.tenant_id AND order_id=NEW.reference_id);
 END IF;
 customer:=app.data_rights_notification_owner(NEW.tenant_id,NEW.template_key,NEW.reference_id);
 IF customer IS NOT NULL THEN
  SELECT * INTO c FROM app.customers WHERE tenant_id=NEW.tenant_id AND id=customer FOR SHARE;
  -- Only erasure blocks delivery; a merely disabled account is merchant-managed.
  erased:=erased OR (FOUND AND (c.email='erased+'||c.id::text||'@erased.invalid' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=NEW.tenant_id AND customer_id=customer)));
 END IF;
 IF erased AND NEW.recipient<>'' THEN
  NEW.recipient:='erased-'||NEW.id::text||'@invalid.test';NEW.status:='dead_letter';NEW.last_error:='PRIVACY_ERASED';NEW.send_token:=NULL;NEW.lease_attempt:=NULL;NEW.lease_worker:=NULL;NEW.sent_at:=NULL;
 END IF;
 RETURN NEW;
END $fn$;

CREATE OR REPLACE FUNCTION app.data_rights_fulfillment_erased_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF EXISTS(SELECT FROM app.data_rights_erased_orders WHERE tenant_id=NEW.tenant_id AND order_id=NEW.order_id) THEN
  IF TG_TABLE_NAME='shipments' THEN NEW.carrier:='[erased]';NEW.tracking_number:='[erased]';
  ELSE
   NEW.reason:='[erased]';
   IF NEW.creation_response IS NOT NULL THEN NEW.creation_response:='{"erased":true}'::jsonb; END IF;
   IF NEW.receive_response IS NOT NULL THEN NEW.receive_response:='{"erased":true}'::jsonb; END IF;
  END IF;
 END IF;
 RETURN NEW;
END $fn$;

-- Replacing ownership only protects future erasures/inserts. Legacy converted
-- notifications can still contain the email of a customer erased before this
-- upgrade. Replay the controlled erasure path only for those durable owners;
-- it scrubs envelopes and retires every dispatch generation under its own role,
-- RLS, lock order and financial-retention guards. Never infer owners by email.
DO $legacy_conversion$
DECLARE erased record;prior_tenant text:=current_setting('app.tenant_id',true);
BEGIN
 FOR erased IN
  SELECT e.* FROM app.data_rights_erasures e
  WHERE EXISTS(SELECT FROM app.quote_conversions q JOIN app.notification_deliveries d
   ON(d.tenant_id,d.reference_id)=(q.tenant_id,q.quote_id)
   WHERE(q.tenant_id,q.customer_id)=(e.tenant_id,e.customer_id)
    AND d.template_key='quote.converted' AND d.recipient<>''
    AND d.last_error IS DISTINCT FROM 'PRIVACY_ERASED')
  ORDER BY e.tenant_id,e.customer_id
 LOOP
  PERFORM set_config('app.tenant_id',erased.tenant_id::text,true);
  PERFORM app.data_rights_erase_customer(erased.customer_id,erased.actor_kind,erased.actor_id);
 END LOOP;
 PERFORM set_config('app.tenant_id',coalesce(prior_tenant,''),true);
END $legacy_conversion$;
