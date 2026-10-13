-- Data rights owns export requests and append-only erasure receipts.
-- Depends on customers/orders/cart/notifications/quotes. Trusted services call capability functions.
-- Invariants: tenant/customer locks before request locks; no credentials exported;
-- snapshot privacy exception changes no financial/identity/state/version columns.
DO $role$
DECLARE m record;
BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_data_rights_executor') THEN CREATE ROLE app_data_rights_executor NOLOGIN; END IF;
 ALTER ROLE app_data_rights_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 -- pg_auth_members is cluster-wide and a superuser's plain REVOKE skips grants
 -- recorded by another grantor (as 016): revoke each membership GRANTED BY it.
 FOR m IN SELECT parent.rolname parent_name,grantor.rolname grantor_name FROM pg_auth_members a
  JOIN pg_roles parent ON parent.oid=a.roleid JOIN pg_roles grantor ON grantor.oid=a.grantor
  WHERE a.member='app_data_rights_executor'::regrole LOOP
  EXECUTE format('REVOKE %I FROM app_data_rights_executor GRANTED BY %I CASCADE',m.parent_name,m.grantor_name); END LOOP;
 FOR m IN SELECT child.rolname child_name,grantor.rolname grantor_name FROM pg_auth_members a
  JOIN pg_roles child ON child.oid=a.member JOIN pg_roles grantor ON grantor.oid=a.grantor
  WHERE a.roleid='app_data_rights_executor'::regrole LOOP
  EXECUTE format('REVOKE app_data_rights_executor FROM %I GRANTED BY %I CASCADE',m.child_name,m.grantor_name); END LOOP;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_data_rights_executor'::regrole OR roleid='app_data_rights_executor'::regrole) THEN
  RAISE EXCEPTION 'PRIVACY_EXECUTOR_MEMBERSHIP_RESIDUAL'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_data_rights_executor;
REVOKE CREATE ON SCHEMA app FROM app_data_rights_executor;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_data_rights_executor;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM app_data_rights_executor;
DO $columns$
DECLARE t record; cols text;
BEGIN
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO cols FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM app_data_rights_executor',cols,cols,cols,cols,t.relname);
 END LOOP;
END $columns$;
CREATE TABLE app.data_rights_exports(
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),id uuid NOT NULL,customer_id uuid NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN('pending','ready','consumed','revoked')),
 token_hash bytea CHECK(octet_length(token_hash)=32),content_hash bytea CHECK(octet_length(content_hash)=32),
 expires_at timestamptz NOT NULL DEFAULT date_trunc('second',clock_timestamp())+interval '24 hours',
 download_expires_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 CHECK((status='pending' AND token_hash IS NULL AND content_hash IS NULL) OR status='revoked'
  OR (status IN('ready','consumed') AND token_hash IS NOT NULL AND content_hash IS NOT NULL AND download_expires_at IS NOT NULL AND download_expires_at<=expires_at))
);
CREATE INDEX data_rights_exports_customer ON app.data_rights_exports(tenant_id,customer_id,id);
CREATE TABLE app.data_rights_erasures(
 tenant_id uuid NOT NULL,customer_id uuid NOT NULL,actor_kind text NOT NULL CHECK(actor_kind IN('user','api_key','service')),
 actor_id uuid NOT NULL,erased_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 scope text NOT NULL DEFAULT 'customer.profile,addresses,credentials,b2b,order_snapshots,exports',
 PRIMARY KEY(tenant_id,customer_id),FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id)
);
DO $rls$
DECLARE n text;r text;
BEGIN
 FOREACH n IN ARRAY ARRAY['data_rights_exports','data_rights_erasures'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',n);EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',n);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC',n);
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP EXECUTE format('REVOKE ALL ON app.%I FROM %I',n,r); END LOOP;
 END LOOP;
 FOREACH n IN ARRAY ARRAY['data_rights_exports','data_rights_erasures','customers','customer_addresses','customer_credentials','buyer_company_members','orders','order_lines','order_adjustments','carts','cart_lines','audit_events','quote_lines','idempotency_records','customer_auth_tokens','customer_sessions','customer_guest_order_links'] LOOP
  EXECUTE format('CREATE POLICY %I ON app.%I TO app_data_rights_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',
   CASE WHEN n IN('data_rights_exports','data_rights_erasures') THEN 'tenant_scope' ELSE 'data_rights_scope' END,n);
 END LOOP;
END $rls$;
GRANT SELECT ON app.tenants,app.customers,app.customer_addresses,app.buyer_company_members,app.orders,app.order_lines,app.carts,app.cart_lines,app.data_rights_exports,app.data_rights_erasures TO app_data_rights_executor;
GRANT SELECT(tenant_id,customer_id),DELETE ON app.customer_credentials TO app_data_rights_executor;
GRANT DELETE ON app.customer_addresses TO app_data_rights_executor;
GRANT UPDATE(email,display_name,status) ON app.customers TO app_data_rights_executor;
GRANT UPDATE(status) ON app.buyer_company_members TO app_data_rights_executor;
GRANT UPDATE(shipping_snapshot) ON app.orders TO app_data_rights_executor;
GRANT UPDATE(tax_snapshot) ON app.order_lines TO app_data_rights_executor;
GRANT INSERT,UPDATE ON app.data_rights_exports TO app_data_rights_executor;
GRANT INSERT ON app.data_rights_erasures,app.audit_events TO app_data_rights_executor;
-- Privacy owns only the contact scrub and dispatch retirement capability.
GRANT SELECT ON app.notification_deliveries TO app_data_rights_executor;
GRANT SELECT(tenant_id,id,event_type,delivery_target,payload,status,lease_owner,lease_until,last_error) ON app.outbox_events TO app_data_rights_executor;
GRANT UPDATE(recipient,status,last_error,send_token,lease_attempt,lease_worker,sent_at,updated_at) ON app.notification_deliveries TO app_data_rights_executor;
GRANT UPDATE(status,lease_owner,lease_until,last_error) ON app.outbox_events TO app_data_rights_executor;
CREATE POLICY data_rights_scope ON app.notification_deliveries TO app_data_rights_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
-- Quote approval mail goes to quotes.accepted_by; that ID, never an email, is ownership.
GRANT SELECT(tenant_id,id,accepted_by) ON app.quotes TO app_data_rights_executor;
CREATE POLICY data_rights_scope ON app.quotes TO app_data_rights_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
-- Every other declared customer-owned copy (C-PII registry identity paths):
-- checkout address/results via order, accepted-quote snapshots, customer-actor
-- command receipts, order adjustment text and customer-auth artifacts.
GRANT SELECT(tenant_id,id,order_id,start_result,final_result),UPDATE(address_snapshot,start_result,final_result) ON app.checkout_sessions TO app_data_rights_executor;
GRANT SELECT(tenant_id,event_kind,event_key,checkout_id,result),UPDATE(result) ON app.checkout_attempts TO app_data_rights_executor;
GRANT SELECT(snapshot),UPDATE(snapshot) ON app.quotes TO app_data_rights_executor;
GRANT SELECT(tenant_id,quote_id,line_no),UPDATE(description,snapshot) ON app.quote_lines TO app_data_rights_executor;
GRANT SELECT(tenant_id,id,order_id),UPDATE(reason) ON app.order_adjustments TO app_data_rights_executor;
GRANT SELECT(tenant_id,actor_kind,actor_id,operation,idempotency_key,response_body,response_body_reference),UPDATE(response_body,response_body_reference) ON app.idempotency_records TO app_data_rights_executor;
GRANT SELECT(tenant_id,customer_id),DELETE ON app.customer_auth_tokens,app.customer_sessions,app.customer_guest_order_links TO app_data_rights_executor;
CREATE POLICY data_rights_scope ON app.outbox_events TO app_data_rights_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND delivery_target='notifications.email');

CREATE FUNCTION app.data_rights_context() RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid;
BEGIN
 BEGIN t:=nullif(current_setting('app.tenant_id',true),'')::uuid; EXCEPTION WHEN invalid_text_representation THEN RAISE EXCEPTION 'PRIVACY_TENANT_REQUIRED' USING ERRCODE='42501'; END;
 IF t IS NULL OR NOT EXISTS(SELECT FROM app.tenants WHERE id=t AND status='active') OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'PRIVACY_ACTIVE_TENANT_READ_COMMITTED_REQUIRED' USING ERRCODE='42501'; END IF;
 RETURN t;
END $fn$;
CREATE FUNCTION app.data_rights_tax_scrub(p jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT '{"erased":true}'::jsonb;
$fn$;
-- Quote snapshot keeps only calculator money/currency facts the quote columns
-- already hold; all free text/destination/lines are dropped.
CREATE FUNCTION app.data_rights_quote_scrub(p jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT coalesce((SELECT jsonb_object_agg(k,v) FROM jsonb_each(p) e(k,v)
  WHERE k IN('calculator','currency','subtotal','tax_amount','shipping_amount','total','calculated_at')),'{}'::jsonb)||'{"erased":true}'::jsonb;
$fn$;
-- The ONLY values the executor may write into other modules' immutable copies.
-- Keyed by table; missing/NULL copies stay NULL so shape CHECKs still hold.
CREATE FUNCTION app.data_rights_scrub_values(p_table text,p_old jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT CASE p_table
  WHEN 'checkout_sessions' THEN jsonb_build_object('address_snapshot','{"erased":true}'::jsonb,
   'start_result',CASE WHEN jsonb_typeof(p_old->'start_result')='object' THEN '{"erased":true}'::jsonb ELSE 'null'::jsonb END,
   'final_result',CASE WHEN jsonb_typeof(p_old->'final_result')='object' THEN '{"erased":true}'::jsonb ELSE 'null'::jsonb END)
  WHEN 'checkout_attempts' THEN jsonb_build_object('result',CASE WHEN jsonb_typeof(p_old->'result')='object' THEN '{"erased":true}'::jsonb ELSE 'null'::jsonb END)
  WHEN 'quotes' THEN jsonb_build_object('snapshot',app.data_rights_quote_scrub(p_old->'snapshot'))
  WHEN 'quote_lines' THEN jsonb_build_object('description','[erased]','snapshot','{"erased":true}'::jsonb)
  WHEN 'order_adjustments' THEN jsonb_build_object('reason','[erased]')
  WHEN 'idempotency_records' THEN jsonb_build_object(
   'response_body',CASE WHEN jsonb_typeof(p_old->'response_body')='object' THEN '{"erased":true}'::jsonb ELSE 'null'::jsonb END,
   'response_body_reference',CASE WHEN jsonb_typeof(p_old->'response_body_reference')='string' THEN '"erased"'::jsonb ELSE 'null'::jsonb END)
 END;
$fn$;
-- Customer owning a notification by business reference: order.customer_id or
-- quote.accepted_by. Recipient text never establishes ownership.
-- ponytail: per-row lookup over the tenant's deliveries during erasure; add an
-- owner column on notification_deliveries if tenant notification volume makes erasure slow.
CREATE FUNCTION app.data_rights_notification_owner(p_tenant uuid,p_template text,p_reference uuid) RETURNS uuid LANGUAGE sql STABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT CASE WHEN p_template IN('order.paid','order.shipped','order.refund') THEN (SELECT customer_id FROM app.orders WHERE tenant_id=p_tenant AND id=p_reference)
  WHEN p_template='quote.approved' THEN (SELECT accepted_by FROM app.quotes WHERE tenant_id=p_tenant AND id=p_reference) END;
$fn$;
CREATE OR REPLACE FUNCTION app.orders_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF TG_OP='UPDATE' AND current_user='app_data_rights_executor' THEN
  IF (to_jsonb(NEW)-'shipping_snapshot') IS DISTINCT FROM (to_jsonb(OLD)-'shipping_snapshot')
   OR NEW.shipping_snapshot IS DISTINCT FROM jsonb_build_object('erased',true,'amount_minor',OLD.shipping_minor,'currency',OLD.currency) THEN
   RAISE EXCEPTION 'ORDER_PRIVACY_SCRUB_ONLY' USING ERRCODE='55000'; END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='INSERT' THEN
  IF current_user<>'app_orders_executor' OR NEW.state<>'pending_payment' OR NEW.version<>1
   OR NEW.sealed OR NEW.refunded_minor<>0 THEN
   RAISE EXCEPTION 'ORDER_CONTROLLED_COMMAND_REQUIRED' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF (to_jsonb(NEW)-ARRAY['state','version','sealed','refunded_minor','updated_at']) IS DISTINCT FROM
    (to_jsonb(OLD)-ARRAY['state','version','sealed','refunded_minor','updated_at'])
   OR (OLD.sealed AND NOT NEW.sealed) THEN
  RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 IF (OLD.state IS DISTINCT FROM NEW.state OR OLD.refunded_minor IS DISTINCT FROM NEW.refunded_minor
  OR OLD.sealed IS DISTINCT FROM NEW.sealed OR OLD.version IS DISTINCT FROM NEW.version)
  AND current_user<>'app_orders_executor' THEN
  RAISE EXCEPTION 'ORDER_CONTROLLED_COMMAND_REQUIRED' USING ERRCODE='55000';
 END IF;
 IF OLD.state IS DISTINCT FROM NEW.state OR OLD.refunded_minor IS DISTINCT FROM NEW.refunded_minor THEN
  IF NOT OLD.sealed OR NOT NEW.sealed OR NEW.version<>OLD.version+1 OR NOT (
   (OLD.state='pending_payment' AND NEW.state IN('paid','cancelled'))
   OR (OLD.state='paid' AND NEW.state IN('partially_fulfilled','fulfilled','partially_refunded','refunded'))
   OR (OLD.state='partially_fulfilled' AND NEW.state IN('fulfilled','partially_refunded','refunded'))
   OR (OLD.state='fulfilled' AND NEW.state IN('partially_refunded','refunded'))
   OR (OLD.state='partially_refunded' AND NEW.state IN('partially_refunded','refunded')))
   OR NEW.refunded_minor<OLD.refunded_minor
   OR (NEW.state IN('partially_refunded','refunded') AND NEW.refunded_minor<=OLD.refunded_minor) THEN
   RAISE EXCEPTION 'ORDER_INVALID_TRANSITION' USING ERRCODE='55000';
  END IF;
 ELSE
  IF NEW.version<>OLD.version THEN RAISE EXCEPTION 'ORDER_INVALID_VERSION' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $fn$;
-- Keep the existing SD insert guard and its parent-write protocol. An INVOKER
-- update guard sees the actual privacy executor, never a forgeable caller GUC.
DROP TRIGGER order_lines_snapshot_guard ON app.order_lines;
CREATE TRIGGER order_lines_snapshot_guard BEFORE INSERT ON app.order_lines FOR EACH ROW EXECUTE FUNCTION app.orders_child_guard();
CREATE FUNCTION app.data_rights_line_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF TG_OP='UPDATE' AND current_user='app_data_rights_executor'
  AND (to_jsonb(NEW)-'tax_snapshot') IS NOT DISTINCT FROM (to_jsonb(OLD)-'tax_snapshot')
  AND NEW.tax_snapshot IS NOT DISTINCT FROM app.data_rights_tax_scrub(OLD.tax_snapshot) THEN RETURN NEW; END IF;
 RAISE EXCEPTION 'ORDER_SNAPSHOT_IMMUTABLE' USING ERRCODE='55000';
END $fn$;
CREATE TRIGGER order_lines_privacy_guard BEFORE UPDATE OR DELETE ON app.order_lines FOR EACH ROW EXECUTE FUNCTION app.data_rights_line_guard();
CREATE FUNCTION app.data_rights_request_export(p_id uuid,p_customer uuid,p_actor_kind text,p_actor_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context(); c app.customers;r app.data_rights_exports;
BEGIN
 IF p_id IS NULL OR p_customer IS NULL OR p_actor_id IS NULL OR p_actor_kind NOT IN('user','api_key','service') OR p_actor_kind IS NULL THEN RAISE EXCEPTION 'PRIVACY_INVALID' USING ERRCODE='22023'; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=p_customer FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF c.status<>'active' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=p_customer) THEN RAISE EXCEPTION 'PRIVACY_ERASED' USING ERRCODE='55000'; END IF;
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF FOUND THEN
  IF r.customer_id<>p_customer THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
  RETURN to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'];
 END IF;
 IF (SELECT count(*) FROM app.data_rights_exports WHERE tenant_id=t AND customer_id=p_customer AND status IN('pending','ready') AND expires_at>clock_timestamp())>=10 THEN RAISE EXCEPTION 'PRIVACY_REQUEST_LIMIT' USING ERRCODE='55000'; END IF;
 INSERT INTO app.data_rights_exports(tenant_id,id,customer_id) VALUES(t,p_id,p_customer) RETURNING * INTO r;
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,payload) VALUES(t,p_actor_kind,p_actor_id,'data_rights.export.request','customer',p_customer,jsonb_build_object('request_id',p_id,'scope','customer'));
 RETURN to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'];
END $fn$;
CREATE FUNCTION app.data_rights_status(p_id uuid,p_customer uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();r app.data_rights_exports;
BEGIN
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id AND customer_id=p_customer;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 RETURN to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'];
END $fn$;
-- Internal capability always locks customer before request, including completion.
CREATE FUNCTION app.data_rights_lock_export(p_id uuid) RETURNS app.data_rights_exports LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();r app.data_rights_exports;c app.customers;
BEGIN
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=r.customer_id FOR UPDATE;
 SELECT * INTO r FROM app.data_rights_exports WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF c.status<>'active' OR r.status IN('consumed','revoked') OR r.expires_at<=clock_timestamp()
  OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=t AND customer_id=c.id) THEN RAISE EXCEPTION 'PRIVACY_REVOKED_EXPIRED' USING ERRCODE='55000'; END IF;
 RETURN r;
END $fn$;
CREATE FUNCTION app.data_rights_build_export(p_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id); j jsonb; n bigint;
BEGIN
 IF r.status<>'pending' THEN RAISE EXCEPTION 'PRIVACY_NOT_PENDING' USING ERRCODE='55000'; END IF;
 SELECT (SELECT count(*) FROM app.customer_addresses WHERE tenant_id=r.tenant_id AND customer_id=r.customer_id)
  +(SELECT count(*) FROM app.orders WHERE tenant_id=r.tenant_id AND customer_id=r.customer_id)
  +(SELECT count(*) FROM app.order_lines l JOIN app.orders o ON(o.tenant_id,o.id)=(l.tenant_id,l.order_id) WHERE o.tenant_id=r.tenant_id AND o.customer_id=r.customer_id)
  +(SELECT count(*) FROM app.carts WHERE tenant_id=r.tenant_id AND customer_id=r.customer_id)
  +(SELECT count(*) FROM app.cart_lines l JOIN app.carts c ON(c.tenant_id,c.id)=(l.tenant_id,l.cart_id) WHERE c.tenant_id=r.tenant_id AND c.customer_id=r.customer_id) INTO n;
 IF n>10000 THEN RAISE EXCEPTION 'PRIVACY_EXPORT_RECORD_LIMIT' USING ERRCODE='22023'; END IF;
 SELECT jsonb_build_object('schema_version',1,'customer',jsonb_build_object('id',c.id,'email',c.email,'display_name',c.display_name,'status',c.status,'created_at',c.created_at),
 'addresses',coalesce((SELECT jsonb_agg(to_jsonb(a)-ARRAY['tenant_id','customer_id'] ORDER BY a.id) FROM app.customer_addresses a WHERE a.tenant_id=r.tenant_id AND a.customer_id=r.customer_id),'[]'::jsonb),
 'orders',coalesce((SELECT jsonb_agg(jsonb_build_object('id',o.id,'order_number',o.order_number,'currency',o.currency,'total_minor',o.total_minor,'state',o.state,'shipping_snapshot',o.shipping_snapshot,'created_at',o.created_at,
 'lines',coalesce((SELECT jsonb_agg(jsonb_build_object('sku',l.sku,'quantity',l.quantity,'unit_price_minor',l.unit_price_minor,'line_net_minor',l.line_net_minor,'line_tax_minor',l.line_tax_minor,'line_total_minor',l.line_total_minor,'tax_snapshot',l.tax_snapshot) ORDER BY l.id) FROM app.order_lines l WHERE (l.tenant_id,l.order_id)=(o.tenant_id,o.id)),'[]'::jsonb)) ORDER BY o.id) FROM app.orders o WHERE o.tenant_id=r.tenant_id AND o.customer_id=r.customer_id),'[]'::jsonb),
 'carts',coalesce((SELECT jsonb_agg(jsonb_build_object('id',ca.id,'currency',ca.currency,'lines',coalesce((SELECT jsonb_agg(jsonb_build_object('sku_id',l.sku_id,'quantity',l.quantity) ORDER BY l.sku_id) FROM app.cart_lines l WHERE (l.tenant_id,l.cart_id)=(ca.tenant_id,ca.id)),'[]'::jsonb)) ORDER BY ca.id) FROM app.carts ca WHERE ca.tenant_id=r.tenant_id AND ca.customer_id=r.customer_id),'[]'::jsonb)) INTO j
 FROM app.customers c WHERE (c.tenant_id,c.id)=(r.tenant_id,r.customer_id);
 IF octet_length(j::text)>10485760 THEN RAISE EXCEPTION 'PRIVACY_EXPORT_BYTES_LIMIT' USING ERRCODE='22023'; END IF;
 RETURN j;
END $fn$;
CREATE FUNCTION app.data_rights_worker_info(p_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id);
BEGIN RETURN (to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'])||jsonb_build_object('content_hash',encode(r.content_hash,'hex')); END $fn$;
CREATE FUNCTION app.data_rights_download_deadline(p_id uuid) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id);
BEGIN RETURN least(r.expires_at,date_trunc('second',clock_timestamp())+interval '15 minutes'); END $fn$;
CREATE FUNCTION app.data_rights_complete_export(p_id uuid,p_token_hash bytea,p_content_hash bytea,p_download_expires_at timestamptz) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id);
BEGIN
 IF r.status<>'pending' THEN RAISE EXCEPTION 'PRIVACY_NOT_PENDING' USING ERRCODE='55000'; END IF;
 IF p_download_expires_at IS NULL OR p_download_expires_at<=clock_timestamp() OR p_download_expires_at>clock_timestamp()+interval '15 minutes' OR p_download_expires_at>r.expires_at THEN RAISE EXCEPTION 'PRIVACY_INVALID_DEADLINE' USING ERRCODE='22023'; END IF;
 IF octet_length(p_token_hash) IS DISTINCT FROM 32 OR octet_length(p_content_hash) IS DISTINCT FROM 32 THEN RAISE EXCEPTION 'PRIVACY_INVALID_HASH' USING ERRCODE='22023'; END IF;
 UPDATE app.data_rights_exports SET status='ready',token_hash=p_token_hash,content_hash=p_content_hash,download_expires_at=p_download_expires_at WHERE tenant_id=r.tenant_id AND id=r.id RETURNING * INTO r;
 RETURN to_jsonb(r)-ARRAY['tenant_id','token_hash','content_hash'];
END $fn$;
CREATE FUNCTION app.data_rights_download_info(p_id uuid,p_customer uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id);
BEGIN
 IF r.customer_id IS DISTINCT FROM p_customer THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.status<>'ready' OR r.download_expires_at<=clock_timestamp() THEN RAISE EXCEPTION 'PRIVACY_NOT_READY' USING ERRCODE='55000'; END IF;
 RETURN jsonb_build_object('id',r.id,'customer_id',r.customer_id,'status',r.status,'download_expires_at',r.download_expires_at,'content_hash',encode(r.content_hash,'hex'));
END $fn$;
CREATE FUNCTION app.data_rights_consume_export(p_id uuid,p_customer uuid,p_token_hash bytea) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE r app.data_rights_exports:=app.data_rights_lock_export(p_id);
BEGIN
 IF r.customer_id IS DISTINCT FROM p_customer THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.status<>'ready' OR r.download_expires_at<=clock_timestamp() OR r.token_hash IS DISTINCT FROM p_token_hash THEN RAISE EXCEPTION 'PRIVACY_INVALID_CAPABILITY' USING ERRCODE='55000'; END IF;
 UPDATE app.data_rights_exports SET status='consumed' WHERE tenant_id=r.tenant_id AND id=r.id;
 RETURN jsonb_build_object('id',r.id,'status','consumed');
END $fn$;
CREATE FUNCTION app.data_rights_erase_customer(p_customer uuid,p_actor_kind text,p_actor_id uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.data_rights_context();c app.customers; requests jsonb; naddr integer;ncred integer;nb2b integer;norder integer;nline integer;nnotification integer;ncheckout integer;nattempt integer;nquote integer;nquoteline integer;nadjust integer;nreceipt integer;nauth integer;n integer;
BEGIN
 IF p_customer IS NULL OR p_actor_id IS NULL OR p_actor_kind IS NULL OR p_actor_kind NOT IN('user','api_key','service') THEN RAISE EXCEPTION 'PRIVACY_INVALID' USING ERRCODE='22023'; END IF;
 -- Existing financial writers lock order before notification fanout locks customer.
 PERFORM id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer ORDER BY id FOR UPDATE;
 SELECT * INTO c FROM app.customers WHERE tenant_id=t AND id=p_customer FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRIVACY_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 -- Catch orders committed while the first pass waited for customer admission.
 -- Fail atomically rather than wait on a transition that now needs our customer.
 PERFORM id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer ORDER BY id FOR UPDATE NOWAIT;
 PERFORM id FROM app.data_rights_exports WHERE tenant_id=t AND customer_id=p_customer ORDER BY id FOR UPDATE;
 SELECT coalesce(jsonb_agg(id ORDER BY id),'[]'::jsonb) INTO requests FROM app.data_rights_exports WHERE tenant_id=t AND customer_id=p_customer;
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
 PERFORM s.id FROM app.checkout_sessions s WHERE s.tenant_id=t AND s.order_id IN(SELECT id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer) ORDER BY s.id FOR UPDATE NOWAIT;
 PERFORM a.event_key FROM app.checkout_attempts a JOIN app.checkout_sessions s ON(s.tenant_id,s.id)=(a.tenant_id,a.checkout_id)
  WHERE s.tenant_id=t AND s.order_id IN(SELECT id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer) ORDER BY a.event_kind,a.event_key FOR UPDATE OF a NOWAIT;
 PERFORM id FROM app.quotes WHERE tenant_id=t AND accepted_by=p_customer ORDER BY id FOR UPDATE NOWAIT;
 PERFORM operation FROM app.idempotency_records WHERE tenant_id=t AND actor_kind='customer' AND actor_id=p_customer ORDER BY operation,idempotency_key FOR UPDATE NOWAIT;
 UPDATE app.checkout_attempts a SET result='{"erased":true}' FROM app.checkout_sessions s WHERE (s.tenant_id,s.id)=(a.tenant_id,a.checkout_id)
  AND s.tenant_id=t AND s.order_id IN(SELECT id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer) AND a.result IS NOT NULL;GET DIAGNOSTICS nattempt=ROW_COUNT;
 ncheckout:=0; -- probe: checkout scrub removed
 UPDATE app.quote_lines l SET description='[erased]',snapshot='{"erased":true}' FROM app.quotes q
  WHERE (q.tenant_id,q.id)=(l.tenant_id,l.quote_id) AND q.tenant_id=t AND q.accepted_by=p_customer;GET DIAGNOSTICS nquoteline=ROW_COUNT;
 UPDATE app.quotes SET snapshot=app.data_rights_quote_scrub(snapshot) WHERE tenant_id=t AND accepted_by=p_customer;GET DIAGNOSTICS nquote=ROW_COUNT;
 UPDATE app.order_adjustments SET reason='[erased]' WHERE tenant_id=t AND order_id IN(SELECT id FROM app.orders WHERE tenant_id=t AND customer_id=p_customer);GET DIAGNOSTICS nadjust=ROW_COUNT;
 UPDATE app.idempotency_records SET
  response_body=CASE WHEN response_body IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END,
  response_body_reference=CASE WHEN response_body_reference IS NULL THEN NULL ELSE 'erased' END
  WHERE tenant_id=t AND actor_kind='customer' AND actor_id=p_customer AND (response_body IS NOT NULL OR response_body_reference IS NOT NULL);GET DIAGNOSTICS nreceipt=ROW_COUNT;
 -- Customer-auth artifacts: pending tokens (email copy, pending PHC), sessions
 -- and mailbox-proven guest links. Account admission counters are unowned.
 DELETE FROM app.customer_auth_tokens WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS nauth=ROW_COUNT;
 DELETE FROM app.customer_sessions WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS n=ROW_COUNT;nauth:=nauth+n;
 DELETE FROM app.customer_guest_order_links WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS n=ROW_COUNT;nauth:=nauth+n;
 DELETE FROM app.customer_addresses WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS naddr=ROW_COUNT;
 DELETE FROM app.customer_credentials WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS ncred=ROW_COUNT;
 UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=t AND customer_id=p_customer AND status<>'revoked';GET DIAGNOSTICS nb2b=ROW_COUNT;
 UPDATE app.orders SET shipping_snapshot=jsonb_build_object('erased',true,'amount_minor',shipping_minor,'currency',currency) WHERE tenant_id=t AND customer_id=p_customer;GET DIAGNOSTICS norder=ROW_COUNT;
 UPDATE app.order_lines l SET tax_snapshot=app.data_rights_tax_scrub(l.tax_snapshot) FROM app.orders o WHERE (o.tenant_id,o.id)=(l.tenant_id,l.order_id) AND o.tenant_id=t AND o.customer_id=p_customer;GET DIAGNOSTICS nline=ROW_COUNT;
 UPDATE app.customers SET email='erased+'||id::text||'@erased.invalid',display_name='[erased]',status='disabled' WHERE tenant_id=t AND id=p_customer;
 INSERT INTO app.data_rights_erasures(tenant_id,customer_id,actor_kind,actor_id) VALUES(t,p_customer,p_actor_kind,p_actor_id) ON CONFLICT DO NOTHING;
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,payload) VALUES(t,p_actor_kind,p_actor_id,'data_rights.customer.erase','customer',p_customer,jsonb_build_object('scope','customer','addresses',naddr,'credentials',ncred,'b2b',nb2b,'orders',norder,'order_lines',nline,'notifications',nnotification,'checkouts',ncheckout,'checkout_attempts',nattempt,'quotes',nquote,'quote_lines',nquoteline,'order_adjustments',nadjust,'command_receipts',nreceipt,'customer_auth',nauth));
 RETURN jsonb_build_object('customer_id',p_customer,'requests',requests,'counts',jsonb_build_object('addresses',naddr,'credentials',ncred,'b2b',nb2b,'orders',norder,'order_lines',nline,'notifications',nnotification,'checkouts',ncheckout,'checkout_attempts',nattempt,'quotes',nquote,'quote_lines',nquoteline,'order_adjustments',nadjust,'command_receipts',nreceipt,'customer_auth',nauth));
END $fn$;
-- Immutable receipt is the restore/re-erasure barrier; no raw PII is retained.
CREATE FUNCTION app.data_rights_erasure_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN RAISE EXCEPTION 'PRIVACY_ERASURE_JOURNAL_IMMUTABLE' USING ERRCODE='55000'; END $fn$;
CREATE TRIGGER data_rights_erasure_guard BEFORE UPDATE OR DELETE ON app.data_rights_erasures FOR EACH ROW EXECUTE FUNCTION app.data_rights_erasure_guard();
-- No future password writer can resurrect an erased account, even if it knows
-- an old ID. Customer lock serializes credential insert with erasure.
CREATE FUNCTION app.data_rights_credential_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $fn$
DECLARE c app.customers;
BEGIN
 PERFORM set_config('app.tenant_id',NEW.tenant_id::text,true);
 SELECT * INTO c FROM app.customers WHERE tenant_id=NEW.tenant_id AND id=NEW.customer_id FOR UPDATE;
 -- Missing/foreign parents remain subject to the named composite FK; only
 -- an existing parent can carry a privacy lifecycle prohibition.
 IF NOT FOUND THEN RETURN NEW; END IF;
 IF c.status<>'active' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=NEW.tenant_id AND customer_id=NEW.customer_id) THEN
  RAISE EXCEPTION 'PRIVACY_CUSTOMER_DISABLED' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_credential_guard BEFORE INSERT OR UPDATE ON app.customer_credentials FOR EACH ROW EXECUTE FUNCTION app.data_rights_credential_guard();
CREATE TRIGGER data_rights_address_guard BEFORE INSERT OR UPDATE ON app.customer_addresses FOR EACH ROW EXECUTE FUNCTION app.data_rights_credential_guard();
CREATE FUNCTION app.data_rights_order_customer_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $fn$
DECLARE c app.customers;
BEGIN
 IF NEW.customer_id IS NULL THEN RETURN NEW; END IF;
 PERFORM set_config('app.tenant_id',NEW.tenant_id::text,true);
 SELECT * INTO c FROM app.customers WHERE tenant_id=NEW.tenant_id AND id=NEW.customer_id FOR SHARE;
 -- Missing/foreign parents remain subject to the named composite FK; only
 -- an existing parent can carry a privacy lifecycle prohibition.
 IF NOT FOUND THEN RETURN NEW; END IF;
 IF c.status<>'active' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=NEW.tenant_id AND customer_id=NEW.customer_id) THEN
  RAISE EXCEPTION 'PRIVACY_CUSTOMER_DISABLED' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_order_customer_guard BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.data_rights_order_customer_guard();

CREATE FUNCTION app.data_rights_customer_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 -- Only an ERASED customer (holding its own reserved placeholder) is frozen.
 -- A merely disabled account stays a merchant-managed lifecycle state that
 -- customer-auth may re-enable; erasure itself still requires the executor.
 IF OLD.email='erased+'||OLD.id::text||'@erased.invalid'
  AND (NEW.email IS DISTINCT FROM OLD.email OR NEW.display_name IS DISTINCT FROM OLD.display_name OR NEW.status IS DISTINCT FROM OLD.status) THEN
  RAISE EXCEPTION 'PRIVACY_DISABLED_CUSTOMER_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF NEW.email='erased+'||NEW.id::text||'@erased.invalid' AND NEW.email IS DISTINCT FROM OLD.email
  AND (current_user<>'app_data_rights_executor' OR NEW.status<>'disabled' OR NEW.display_name<>'[erased]') THEN
  RAISE EXCEPTION 'PRIVACY_DISABLED_CUSTOMER_IMMUTABLE' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_customer_guard BEFORE UPDATE ON app.customers FOR EACH ROW EXECUTE FUNCTION app.data_rights_customer_guard();
-- Fanout keeps its module contract; privacy adds only authoritative lifecycle
-- admission. Function-local tenant binding supports existing privileged fixtures
-- while the caller's ordinary table RLS still controls the INSERT itself.
CREATE FUNCTION app.data_rights_notification_insert_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $fn$
DECLARE customer uuid;c app.customers;
BEGIN
 IF NEW.template_key NOT IN('order.paid','order.shipped','order.refund','quote.approved') THEN RETURN NEW; END IF;
 PERFORM set_config('app.tenant_id',NEW.tenant_id::text,true);
 customer:=app.data_rights_notification_owner(NEW.tenant_id,NEW.template_key,NEW.reference_id);
 IF customer IS NULL THEN RETURN NEW; END IF;
 SELECT * INTO c FROM app.customers WHERE tenant_id=NEW.tenant_id AND id=customer FOR SHARE;
 IF FOUND AND (c.status<>'active' OR EXISTS(SELECT FROM app.data_rights_erasures WHERE tenant_id=NEW.tenant_id AND customer_id=customer)) AND NEW.recipient<>'' THEN
  NEW.recipient:='erased-'||NEW.id::text||'@invalid.test';NEW.status:='dead_letter';NEW.last_error:='PRIVACY_ERASED';NEW.send_token:=NULL;NEW.lease_attempt:=NULL;NEW.lease_worker:=NULL;NEW.sent_at:=NULL;
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_notification_insert_guard BEFORE INSERT ON app.notification_deliveries FOR EACH ROW EXECUTE FUNCTION app.data_rights_notification_insert_guard();
CREATE FUNCTION app.data_rights_notification_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF OLD.last_error='PRIVACY_ERASED' AND (to_jsonb(NEW)-'updated_at') IS DISTINCT FROM (to_jsonb(OLD)-'updated_at') THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF current_user='app_data_rights_executor' THEN
  IF NEW.recipient<>'erased-'||OLD.id::text||'@invalid.test' OR NEW.status<>'dead_letter' OR NEW.last_error IS DISTINCT FROM 'PRIVACY_ERASED' OR NEW.send_token IS NOT NULL OR NEW.lease_attempt IS NOT NULL OR NEW.lease_worker IS NOT NULL OR NEW.sent_at IS NOT NULL
   OR (to_jsonb(NEW)-ARRAY['recipient','status','last_error','send_token','lease_attempt','lease_worker','sent_at','updated_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['recipient','status','last_error','send_token','lease_attempt','lease_worker','sent_at','updated_at']) THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION_SCRUB_ONLY' USING ERRCODE='55000'; END IF;
 ELSIF (OLD.recipient<>'' AND NEW.recipient IS DISTINCT FROM OLD.recipient) OR NEW.last_error='PRIVACY_ERASED' AND OLD.last_error IS DISTINCT FROM 'PRIVACY_ERASED' THEN
  RAISE EXCEPTION 'PRIVACY_NOTIFICATION_ENVELOPE_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_notification_guard BEFORE UPDATE ON app.notification_deliveries FOR EACH ROW EXECUTE FUNCTION app.data_rights_notification_guard();
CREATE FUNCTION app.data_rights_notification_dispatch_insert_guard() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp SET app.tenant_id='' AS $fn$
BEGIN
 IF NEW.delivery_target<>'notifications.email' OR NEW.event_type<>'notification.dispatch' THEN RETURN NEW; END IF;
 PERFORM set_config('app.tenant_id',NEW.tenant_id::text,true);
 IF EXISTS(SELECT FROM app.notification_deliveries d WHERE d.tenant_id=NEW.tenant_id AND NEW.payload=jsonb_build_object('notification_id',d.id) AND d.last_error='PRIVACY_ERASED') THEN
  NEW.status:='dead_letter';NEW.lease_owner:=NULL;NEW.lease_until:=NULL;NEW.last_error:='PRIVACY_ERASED';
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_notification_dispatch_insert_guard BEFORE INSERT ON app.outbox_events FOR EACH ROW EXECUTE FUNCTION app.data_rights_notification_dispatch_insert_guard();
CREATE FUNCTION app.data_rights_notification_dispatch_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF OLD.delivery_target<>'notifications.email' THEN RETURN NEW; END IF;
 IF OLD.last_error='PRIVACY_ERASED' AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION_DISPATCH_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF NEW.last_error='PRIVACY_ERASED' AND OLD.last_error IS DISTINCT FROM 'PRIVACY_ERASED' THEN
  IF current_user<>'app_data_rights_executor' OR NEW.status IS DISTINCT FROM (CASE WHEN OLD.status='delivered' THEN 'delivered' ELSE 'dead_letter' END) OR NEW.lease_owner IS NOT NULL OR NEW.lease_until IS NOT NULL
   OR (to_jsonb(NEW)-ARRAY['status','lease_owner','lease_until','last_error']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','lease_owner','lease_until','last_error']) THEN RAISE EXCEPTION 'PRIVACY_NOTIFICATION_DISPATCH_SCRUB_ONLY' USING ERRCODE='55000'; END IF;
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_notification_dispatch_guard BEFORE UPDATE ON app.outbox_events FOR EACH ROW EXECUTE FUNCTION app.data_rights_notification_dispatch_guard();
-- Erased placeholders live in a reserved non-routable namespace that only the
-- erased row itself may hold, so no account can pre-register one to block
-- erasure. A trigger (not a CHECK) leaves pre-existing legacy rows valid on
-- upgrade; it fires on insert and on any email change.
CREATE FUNCTION app.data_rights_email_namespace_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF lower(regexp_replace(NEW.email,'^[[:space:]]+|[[:space:]]+$','','g')) LIKE '%@erased.invalid'
  AND NOT (NEW.email='erased+'||NEW.id::text||'@erased.invalid' AND NEW.status='disabled') THEN
  RAISE EXCEPTION 'PRIVACY_RESERVED_EMAIL_NAMESPACE' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_email_namespace_guard BEFORE INSERT OR UPDATE OF email ON app.customers FOR EACH ROW EXECUTE FUNCTION app.data_rights_email_namespace_guard();
-- Owner-module guards keep their full contract for every other role. The
-- executor bypasses them only into this guard, which admits exactly the
-- registered scrub values for UPDATE and refuses every other write.
CREATE FUNCTION app.data_rights_scrub_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF TG_OP<>'UPDATE' OR to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD)||app.data_rights_scrub_values(TG_TABLE_NAME,to_jsonb(OLD)) THEN
  RAISE EXCEPTION 'PRIVACY_SCRUB_ONLY' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $fn$;
DROP TRIGGER checkout_sessions_guard ON app.checkout_sessions;
CREATE TRIGGER checkout_sessions_guard BEFORE INSERT OR UPDATE OR DELETE ON app.checkout_sessions FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.checkout_guard();
DROP TRIGGER checkout_attempts_guard ON app.checkout_attempts;
CREATE TRIGGER checkout_attempts_guard BEFORE INSERT OR UPDATE OR DELETE ON app.checkout_attempts FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.checkout_guard();
DROP TRIGGER quote_guard ON app.quotes;
CREATE TRIGGER quote_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quotes FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.guard_quote();
DROP TRIGGER quote_lines_guard ON app.quote_lines;
CREATE TRIGGER quote_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quote_lines FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.guard_quote_child();
DROP TRIGGER order_adjustments_snapshot_guard ON app.order_adjustments;
CREATE TRIGGER order_adjustments_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON app.order_adjustments FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.orders_child_guard();
DROP TRIGGER idempotency_identity ON app.idempotency_records;
CREATE TRIGGER idempotency_identity BEFORE UPDATE ON app.idempotency_records FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.guard_idempotency_identity();
DO $scrub_triggers$ DECLARE n text; BEGIN
 FOREACH n IN ARRAY ARRAY['checkout_sessions','checkout_attempts','quotes','quote_lines','order_adjustments','idempotency_records'] LOOP
  EXECUTE format('CREATE TRIGGER data_rights_scrub_guard BEFORE INSERT OR UPDATE OR DELETE ON app.%I FOR EACH ROW WHEN (current_user=''app_data_rights_executor'') EXECUTE FUNCTION app.data_rights_scrub_guard()',n);
 END LOOP;
END $scrub_triggers$;
-- A checkout still in flight at erasure cannot write a fresh personal copy:
-- later results for a scrubbed session are stored as the erased marker.
CREATE FUNCTION app.data_rights_checkout_erased_guard() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF TG_TABLE_NAME='checkout_sessions' THEN
  IF OLD.address_snapshot='{"erased":true}'::jsonb THEN
   NEW.start_result:=CASE WHEN NEW.start_result IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END;
   NEW.final_result:=CASE WHEN NEW.final_result IS NULL THEN NULL ELSE '{"erased":true}'::jsonb END;
  END IF;
 ELSIF NEW.result IS NOT NULL AND EXISTS(SELECT FROM app.checkout_sessions s WHERE s.tenant_id=NEW.tenant_id AND s.id=NEW.checkout_id AND s.address_snapshot='{"erased":true}'::jsonb) THEN
  NEW.result:='{"erased":true}'::jsonb;
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER data_rights_checkout_erased_guard BEFORE UPDATE ON app.checkout_sessions FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.data_rights_checkout_erased_guard();
CREATE TRIGGER data_rights_checkout_erased_guard BEFORE UPDATE ON app.checkout_attempts FOR EACH ROW WHEN (current_user<>'app_data_rights_executor') EXECUTE FUNCTION app.data_rights_checkout_erased_guard();
-- Ownership is the security boundary. No login/member can become this role.
GRANT CREATE ON SCHEMA app TO app_data_rights_executor;
DO $functions$
DECLARE f record;r text;
BEGIN
 FOR f IN SELECT p.oid::regprocedure AS signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND starts_with(p.proname,'data_rights_') LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_data_rights_executor',f.signature);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.signature);
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_data_rights_executor' LOOP EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.signature,r); END LOOP;
 END LOOP;
END $functions$;
REVOKE CREATE ON SCHEMA app FROM app_data_rights_executor;
GRANT EXECUTE ON FUNCTION app.data_rights_request_export(uuid,uuid,text,uuid),app.data_rights_status(uuid,uuid),app.data_rights_build_export(uuid),app.data_rights_complete_export(uuid,bytea,bytea,timestamptz),app.data_rights_download_deadline(uuid),app.data_rights_worker_info(uuid),app.data_rights_download_info(uuid,uuid),app.data_rights_consume_export(uuid,uuid,bytea),app.data_rights_erase_customer(uuid,text,uuid) TO app_runtime;
