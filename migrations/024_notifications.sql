-- Responsibility: frozen notification envelopes, dedicated outbox claims and fenced delivery receipts.
-- Dependencies: orders/customers and 011/022 outbox; optional authoritative quotes only.
-- Caller: source-event trigger, notifications worker and scoped admin redrive.
-- Invariants: original events untouched; no resend after ambiguous SMTP; tenant keys throughout.
DO $role$ DECLARE m record; t record; cols text; BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_notifications_executor') THEN CREATE ROLE app_notifications_executor NOLOGIN; END IF;
 ALTER ROLE app_notifications_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 FOR m IN SELECT parent.rolname parent_name,child.rolname child_name,g.rolname grantor_name
 FROM pg_auth_members a JOIN pg_roles parent ON parent.oid=a.roleid JOIN pg_roles child ON child.oid=a.member
 JOIN pg_roles g ON g.oid=a.grantor WHERE a.roleid='app_notifications_executor'::regrole OR a.member='app_notifications_executor'::regrole LOOP
  EXECUTE format('REVOKE %I FROM %I GRANTED BY %I CASCADE',m.parent_name,m.child_name,m.grantor_name);
 END LOOP;
 REVOKE ALL ON SCHEMA app FROM app_notifications_executor;
 REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_notifications_executor;
 REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM app_notifications_executor;
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO cols FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM app_notifications_executor',cols,cols,cols,cols,t.relname);
 END LOOP;
END $role$;
GRANT USAGE ON SCHEMA app TO app_notifications_executor;
CREATE TABLE app.notification_deliveries(
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), id uuid NOT NULL DEFAULT uuidv7(),
 source_event_id uuid NOT NULL, dispatch_event_id uuid,
 recipient text NOT NULL,
 locale text NOT NULL DEFAULT 'en' CHECK(locale IN('en','zh')),
 template_key text NOT NULL CHECK(template_key IN('order.paid','order.shipped','order.refund','quote.sent','quote.approved')),
 reference_id uuid NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN('pending','sending','sent','failed','dead_letter','uncertain')),
 attempt_count integer NOT NULL DEFAULT 0 CHECK(attempt_count>=0), generation integer NOT NULL DEFAULT 1 CHECK(generation>0),
 send_token uuid, lease_attempt integer, lease_worker uuid,
 last_error text CHECK(last_error IS NULL OR (length(last_error) BETWEEN 1 AND 128 AND last_error ~ '^[A-Z0-9_]+$')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),sent_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,source_event_id,recipient,template_key), UNIQUE(tenant_id,dispatch_event_id),
 FOREIGN KEY(tenant_id,source_event_id) REFERENCES app.outbox_events(tenant_id,id),
 FOREIGN KEY(tenant_id,dispatch_event_id) REFERENCES app.outbox_events(tenant_id,id) DEFERRABLE INITIALLY DEFERRED,
 CHECK((recipient='' AND status='dead_letter' AND last_error IS NOT DISTINCT FROM 'NOTIFICATION_RECIPIENT_UNAVAILABLE' AND dispatch_event_id IS NULL) OR (length(recipient) BETWEEN 3 AND 254 AND recipient=lower(btrim(recipient)) AND recipient COLLATE "C" ~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+([.][A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+)*@[A-Za-z0-9.-]+[.][A-Za-z0-9-]+$' AND dispatch_event_id IS NOT NULL)),
 CHECK((status='sending')=(send_token IS NOT NULL)),
 CHECK((status='sent')=(sent_at IS NOT NULL)),
 CHECK((lease_attempt IS NULL AND lease_worker IS NULL) OR (lease_attempt BETWEEN 1 AND 10 AND lease_worker IS NOT NULL))
);
CREATE INDEX notification_failures ON app.notification_deliveries(tenant_id,created_at,id) WHERE status IN('failed','dead_letter','uncertain');
CREATE TABLE app.notification_redrive_requests(
 tenant_id uuid NOT NULL,notification_id uuid NOT NULL,idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128 AND btrim(idempotency_key)<>''),
 request_token uuid NOT NULL DEFAULT uuidv7(), generation integer NOT NULL CHECK(generation>1),dispatch_event_id uuid NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,notification_id,idempotency_key),
 FOREIGN KEY(tenant_id,notification_id) REFERENCES app.notification_deliveries(tenant_id,id),
 FOREIGN KEY(tenant_id,dispatch_event_id) REFERENCES app.outbox_events(tenant_id,id) DEFERRABLE INITIALLY DEFERRED
);
DO $rls$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['notification_deliveries','notification_redrive_requests'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',name);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime,app_notifications_executor',name);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_notifications_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',name);
  EXECUTE format('GRANT SELECT ON app.%I TO app_runtime,app_notifications_executor',name);
 END LOOP;
END $rls$;
GRANT INSERT(tenant_id,id,source_event_id,dispatch_event_id,recipient,locale,template_key,reference_id,status,last_error) ON app.notification_deliveries TO app_notifications_executor;
GRANT UPDATE(dispatch_event_id,status,attempt_count,generation,send_token,lease_attempt,lease_worker,last_error,updated_at,sent_at) ON app.notification_deliveries TO app_notifications_executor;
GRANT INSERT(tenant_id,notification_id,idempotency_key,generation,dispatch_event_id) ON app.notification_redrive_requests TO app_notifications_executor;
GRANT SELECT ON app.outbox_events TO app_notifications_executor;
GRANT INSERT(tenant_id,id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events TO app_notifications_executor;
GRANT UPDATE(status,lease_owner,lease_until,attempts,last_error) ON app.outbox_events TO app_notifications_executor;
GRANT SELECT(tenant_id,id,customer_id,guest_contact) ON app.orders TO app_notifications_executor;
GRANT SELECT(tenant_id,id,email_normalized) ON app.customers TO app_notifications_executor;
CREATE POLICY notifications_orders ON app.orders TO app_notifications_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY notifications_customers ON app.customers TO app_notifications_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY notifications_outbox ON app.outbox_events TO app_notifications_executor USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND delivery_target='notifications.email');
CREATE POLICY notifications_namespace ON app.outbox_events AS RESTRICTIVE FOR INSERT TO app_runtime WITH CHECK(delivery_target<>'notifications.email' AND delivery_target<>'quotes');

-- A later-installed quotes migration must install these same integration ACLs.
DO $quotes$ BEGIN
 IF to_regclass('app.quotes') IS NOT NULL THEN
  EXECUTE 'GRANT SELECT(tenant_id,id,site_id,inquiry_id,buyer_company_id,accepted_by,status,version) ON app.quotes TO app_notifications_executor';
  EXECUTE 'GRANT SELECT(tenant_id,id,site_id,buyer_company_id,buyer_email) ON app.inquiries TO app_notifications_executor';
  EXECUTE 'CREATE POLICY notifications_quotes ON app.quotes TO app_notifications_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)';
  EXECUTE 'CREATE POLICY notifications_inquiries ON app.inquiries TO app_notifications_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)';
 END IF;
END $quotes$;

CREATE FUNCTION app.notification_fanout() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE email text; ref uuid; template text; quote_valid boolean; nid uuid:=uuidv7(); dispatch uuid:=uuidv7(); BEGIN
 IF NEW.delivery_target='orders.events' AND NEW.event_type IN('order.paid','order.fulfilled','order.partially_fulfilled','order.refunded','order.partially_refunded') THEN
  IF coalesce(NEW.payload->>'order_id','') !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$' THEN RETURN NEW; END IF;
  ref:=(NEW.payload->>'order_id')::uuid;
  SELECT coalesce(o.guest_contact->>'email',c.email_normalized) INTO email FROM app.orders o LEFT JOIN app.customers c ON(c.tenant_id,c.id)=(o.tenant_id,o.customer_id) WHERE o.tenant_id=NEW.tenant_id AND o.id=ref;
  template:=CASE NEW.event_type WHEN 'order.paid' THEN 'order.paid' WHEN 'order.fulfilled' THEN 'order.shipped' WHEN 'order.partially_fulfilled' THEN 'order.shipped' ELSE 'order.refund' END;
 ELSIF NEW.delivery_target='quotes' AND NEW.event_type IN('quotes.sent','quotes.approved') THEN
  -- Quote contacts cannot come from caller payload. Optional schema is checked
  -- without parsing any quote SQL until installed and permissioned integration.
  IF to_regclass('app.quotes') IS NULL THEN RETURN NEW; END IF;
  IF NOT has_column_privilege(current_user,'app.quotes','id','SELECT') THEN RETURN NEW; END IF;
  IF coalesce(NEW.payload->>'quote_id','') !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$' THEN RETURN NEW; END IF;
  ref:=(NEW.payload->>'quote_id')::uuid;
  -- The fixed source namespace grants no authority to invent a lifecycle.
  EXECUTE 'SELECT EXISTS(SELECT FROM app.quotes q WHERE q.tenant_id=$1 AND q.id=$2 AND q.status=CASE $3 WHEN ''quotes.sent'' THEN ''sent'' ELSE ''approved'' END AND ($4::text IS NULL OR q.version::text=$4))' INTO quote_valid USING NEW.tenant_id,ref,NEW.event_type,NEW.payload->>'version';
  IF NOT quote_valid THEN RETURN NEW; END IF;
  IF NEW.event_type='quotes.sent' THEN
   EXECUTE 'SELECT i.buyer_email FROM app.quotes q JOIN app.inquiries i ON(i.tenant_id,i.site_id,i.id)=(q.tenant_id,q.site_id,q.inquiry_id) AND i.buyer_company_id=q.buyer_company_id WHERE q.tenant_id=$1 AND q.id=$2' INTO email USING NEW.tenant_id,ref;
  ELSE
   EXECUTE 'SELECT c.email_normalized FROM app.quotes q JOIN app.customers c ON(c.tenant_id,c.id)=(q.tenant_id,q.accepted_by) WHERE q.tenant_id=$1 AND q.id=$2' INTO email USING NEW.tenant_id,ref;
  END IF;
  template:=CASE NEW.event_type WHEN 'quotes.sent' THEN 'quote.sent' ELSE 'quote.approved' END;
 ELSE RETURN NEW;
 END IF;
 email:=lower(btrim(email));
 -- Invalid contacts are visible terminal failures without dispatch or business rollback.
 IF email IS NULL OR length(email)>254 OR email COLLATE "C" !~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+([.][A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+)*@[A-Za-z0-9.-]+[.][A-Za-z0-9-]+$' THEN
  INSERT INTO app.notification_deliveries(tenant_id,id,source_event_id,recipient,locale,template_key,reference_id,status,last_error)
  VALUES(NEW.tenant_id,nid,NEW.id,'','en',template,ref,'dead_letter','NOTIFICATION_RECIPIENT_UNAVAILABLE') ON CONFLICT(tenant_id,source_event_id,recipient,template_key) DO NOTHING;
  RETURN NEW;
 END IF;
 INSERT INTO app.notification_deliveries(tenant_id,id,source_event_id,dispatch_event_id,recipient,locale,template_key,reference_id)
 VALUES(NEW.tenant_id,nid,NEW.id,dispatch,email,'en',template,ref) ON CONFLICT(tenant_id,source_event_id,recipient,template_key) DO NOTHING;
 IF FOUND THEN
  INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key)
  VALUES(NEW.tenant_id,dispatch,'notification.dispatch',jsonb_build_object('notification_id',nid),'notifications.email',nid::text||':1');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER notification_source_fanout AFTER INSERT ON app.outbox_events FOR EACH ROW EXECUTE FUNCTION app.notification_fanout();

CREATE FUNCTION app.claim_notification_outbox(p_tenant uuid,p_worker uuid,p_limit integer,p_lease_seconds integer DEFAULT 60)
RETURNS SETOF app.outbox_events LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'NOTIFICATIONS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 IF p_worker IS NULL OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 1 AND 3600 THEN RAISE EXCEPTION 'NOTIFICATIONS_CLAIM_ARGUMENT' USING ERRCODE='22023'; END IF;
 WITH exhausted AS MATERIALIZED(SELECT e.tenant_id,e.id FROM app.outbox_events e WHERE e.tenant_id=p_tenant AND e.delivery_target='notifications.email' AND e.attempts>=10 AND (e.status IN('pending','failed') OR (e.status='processing' AND e.lease_until<=clock_timestamp())) ORDER BY e.next_attempt_at,e.id LIMIT p_limit FOR UPDATE SKIP LOCKED)
 UPDATE app.outbox_events e SET status='dead_letter',lease_owner=NULL,lease_until=NULL,last_error=coalesce(e.last_error,'OUTBOX_ATTEMPT_LIMIT') FROM exhausted x WHERE(e.tenant_id,e.id)=(x.tenant_id,x.id);
 -- Final crash has unknown external outcome. Never label it known failure.
 UPDATE app.notification_deliveries d SET status=CASE WHEN d.status='sending' THEN 'uncertain' ELSE 'dead_letter' END,send_token=NULL,updated_at=clock_timestamp(),last_error=CASE WHEN d.status='sending' THEN 'SEND_RESULT_UNKNOWN' ELSE 'OUTBOX_ATTEMPT_LIMIT' END
 FROM app.outbox_events e WHERE(d.tenant_id,d.dispatch_event_id)=(e.tenant_id,e.id) AND e.tenant_id=p_tenant AND e.delivery_target='notifications.email' AND e.status='dead_letter' AND d.status IN('pending','failed','sending');
 RETURN QUERY WITH due AS MATERIALIZED(SELECT e.tenant_id,e.id FROM app.outbox_events e WHERE e.tenant_id=p_tenant AND e.delivery_target='notifications.email' AND e.attempts<10 AND ((e.status IN('pending','failed') AND e.next_attempt_at<=clock_timestamp()) OR(e.status='processing' AND e.lease_until<=clock_timestamp())) ORDER BY e.next_attempt_at,e.id LIMIT p_limit FOR UPDATE SKIP LOCKED)
 UPDATE app.outbox_events e SET status='processing',lease_owner=p_worker,lease_until=clock_timestamp()+make_interval(secs=>p_lease_seconds),attempts=e.attempts+1,last_error=NULL FROM due x WHERE(e.tenant_id,e.id)=(x.tenant_id,x.id) RETURNING e.*;
END $$;

CREATE FUNCTION app.notification_begin(p_tenant uuid,p_dispatch_event uuid,p_notification_id uuid) RETURNS app.notification_deliveries LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.notification_deliveries; e app.outbox_events; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'NOTIFICATIONS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 SELECT * INTO e FROM app.outbox_events WHERE tenant_id=p_tenant AND id=p_dispatch_event FOR UPDATE;
 IF NOT FOUND OR e.delivery_target<>'notifications.email' OR e.event_type<>'notification.dispatch' OR e.payload IS DISTINCT FROM jsonb_build_object('notification_id',p_notification_id) OR e.status<>'processing' OR e.lease_until<=clock_timestamp() OR e.lease_owner IS DISTINCT FROM nullif(current_setting('app.outbox_worker',true),'')::uuid OR e.attempts IS DISTINCT FROM nullif(current_setting('app.outbox_attempt',true),'')::integer THEN RAISE EXCEPTION 'NOTIFICATIONS_LEASE_FENCE' USING ERRCODE='42501'; END IF;
 SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=p_tenant AND id=p_notification_id AND dispatch_event_id=p_dispatch_event FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'NOTIFICATIONS_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF d.status='sending' THEN UPDATE app.notification_deliveries SET status='uncertain',send_token=NULL,last_error='SEND_RESULT_UNKNOWN',updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=d.id RETURNING * INTO d;
 ELSIF d.status IN('pending','failed') THEN UPDATE app.notification_deliveries SET status='sending',send_token=uuidv7(),attempt_count=attempt_count+1,lease_attempt=e.attempts,lease_worker=e.lease_owner,last_error=NULL,updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=d.id RETURNING * INTO d;
 END IF;
 RETURN d;
END $$;
CREATE FUNCTION app.notification_finish(p_tenant uuid,p_notification_id uuid,p_token uuid,p_outcome text,p_error text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.notification_deliveries; e app.outbox_events; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'NOTIFICATIONS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 IF p_outcome IS NULL OR p_outcome NOT IN('sent','failed','uncertain') OR (p_outcome<>'sent' AND (p_error IS NULL OR length(p_error) NOT BETWEEN 1 AND 128 OR p_error !~ '^[A-Z0-9_]+$')) THEN RAISE EXCEPTION 'NOTIFICATIONS_FINISH_ARGUMENT' USING ERRCODE='22023'; END IF;
 -- Same lock order as begin: outbox then delivery. Initial lookup is tenant-scoped.
 SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=p_tenant AND id=p_notification_id;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT * INTO e FROM app.outbox_events WHERE tenant_id=p_tenant AND id=d.dispatch_event_id FOR UPDATE;
 SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=p_tenant AND id=p_notification_id FOR UPDATE;
 IF d.status<>'sending' OR d.send_token IS DISTINCT FROM p_token OR e.status<>'processing' OR e.lease_until<=clock_timestamp() OR e.lease_owner IS DISTINCT FROM d.lease_worker OR e.attempts IS DISTINCT FROM d.lease_attempt THEN RETURN false; END IF;
 UPDATE app.notification_deliveries SET status=CASE WHEN p_outcome='failed' AND e.attempts>=10 THEN 'dead_letter' ELSE p_outcome END,send_token=NULL,last_error=CASE WHEN p_outcome='sent' THEN NULL ELSE p_error END,sent_at=CASE WHEN p_outcome='sent' THEN clock_timestamp() ELSE NULL END,updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=d.id;
 RETURN true;
END $$;
CREATE FUNCTION app.notification_redrive(p_tenant uuid,p_notification_id uuid,p_key text) RETURNS app.notification_redrive_requests LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.notification_deliveries;r app.notification_redrive_requests; dispatch uuid:=uuidv7(); BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'NOTIFICATIONS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 IF p_key IS NULL OR length(p_key) NOT BETWEEN 1 AND 128 OR btrim(p_key)='' THEN RAISE EXCEPTION 'NOTIFICATIONS_KEY_ARGUMENT' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM app.notification_deliveries WHERE tenant_id=p_tenant AND id=p_notification_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'NOTIFICATIONS_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT * INTO r FROM app.notification_redrive_requests WHERE tenant_id=p_tenant AND notification_id=p_notification_id AND idempotency_key=p_key;
 IF FOUND THEN RETURN r; END IF;
 IF d.status<>'dead_letter' OR d.recipient='' THEN RAISE EXCEPTION 'NOTIFICATIONS_REDRIVE_STATE' USING ERRCODE='55000'; END IF;
 UPDATE app.notification_deliveries SET status='pending',generation=generation+1,dispatch_event_id=dispatch,send_token=NULL,lease_attempt=NULL,lease_worker=NULL,last_error=NULL,updated_at=clock_timestamp() WHERE tenant_id=p_tenant AND id=p_notification_id RETURNING * INTO d;
 INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key) VALUES(p_tenant,dispatch,'notification.dispatch',jsonb_build_object('notification_id',d.id),'notifications.email',d.id::text||':'||d.generation::text);
 INSERT INTO app.notification_redrive_requests(tenant_id,notification_id,idempotency_key,generation,dispatch_event_id) VALUES(p_tenant,d.id,p_key,d.generation,dispatch) RETURNING * INTO r;
 RETURN r;
END $$;
GRANT CREATE ON SCHEMA app TO app_notifications_executor;
ALTER FUNCTION app.notification_fanout() OWNER TO app_notifications_executor;
ALTER FUNCTION app.claim_notification_outbox(uuid,uuid,integer,integer) OWNER TO app_notifications_executor;
ALTER FUNCTION app.notification_begin(uuid,uuid,uuid) OWNER TO app_notifications_executor;
ALTER FUNCTION app.notification_finish(uuid,uuid,uuid,text,text) OWNER TO app_notifications_executor;
ALTER FUNCTION app.notification_redrive(uuid,uuid,text) OWNER TO app_notifications_executor;
REVOKE CREATE ON SCHEMA app FROM app_notifications_executor;
REVOKE ALL ON FUNCTION app.notification_fanout(),app.claim_notification_outbox(uuid,uuid,integer,integer),app.notification_begin(uuid,uuid,uuid),app.notification_finish(uuid,uuid,uuid,text,text),app.notification_redrive(uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.claim_notification_outbox(uuid,uuid,integer,integer),app.notification_begin(uuid,uuid,uuid),app.notification_finish(uuid,uuid,uuid,text,text) TO app_outbox_worker;
GRANT EXECUTE ON FUNCTION app.notification_redrive(uuid,uuid,text) TO app_runtime;
