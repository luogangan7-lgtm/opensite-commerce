-- Responsibility: Quotes owns existing inquiries, immutable quotes/lines and one approval.
-- Dependencies: identity001, CMS005, reliability011, customers/B2B016; forward migrator.
-- Callers: runtime sqlc commands under an authenticated tenant transaction.
-- Invariants: composite tenant FKs; actor checked in DB; row-lock/version CAS;
-- snapshots never change; state+approval+outbox commit together. Rollback: forward repair.
-- Refuse default grants before creating data or routines. Catalog defaults can
-- otherwise retain column EXECUTE capabilities outside the exact matrix.
DO $defaults$ DECLARE found text; BEGIN
 SELECT string_agg(DISTINCT a.privilege_type,',') INTO found FROM pg_default_acl d
 CROSS JOIN LATERAL (SELECT grantee,privilege_type FROM aclexplode(d.defaclacl)
  EXCEPT SELECT grantee,privilege_type FROM aclexplode(acldefault(d.defaclobjtype,d.defaclrole)) WHERE d.defaclnamespace=0) a
 WHERE d.defaclrole=current_user::regrole AND d.defaclnamespace IN(0,'app'::regnamespace)
 AND(a.grantee=0 OR a.grantee IN(SELECT oid FROM pg_roles WHERE starts_with(rolname,'app_')));
 IF found IS NOT NULL THEN RAISE EXCEPTION 'QUOTES_DEFAULT_PRIVILEGES_UNSAFE %',found; END IF;
END $defaults$;
ALTER TABLE app.inquiries ADD COLUMN buyer_company_id uuid,
 ADD CONSTRAINT inquiries_company_fk FOREIGN KEY(tenant_id,buyer_company_id) REFERENCES app.buyer_companies(tenant_id,id);
CREATE TABLE app.quotes (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 site_id uuid NOT NULL,
 inquiry_id uuid NOT NULL,
 buyer_company_id uuid NOT NULL,
 created_by uuid NOT NULL,
 currency char(3) NOT NULL CHECK(currency::text ~ '^[A-Z]{3}$'),
 subtotal bigint NOT NULL CHECK(subtotal>=0), tax bigint NOT NULL CHECK(tax>=0),
 shipping bigint NOT NULL CHECK(shipping>=0), total bigint NOT NULL CHECK(total>=0),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object' AND octet_length(snapshot::text)<=65536),
 expires_at timestamptz NOT NULL CHECK(isfinite(expires_at)),
 status text NOT NULL DEFAULT 'draft' CHECK(status IN('draft','sent','accepted_pending_approval','approved','rejected','withdrawn','expired')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 accepted_by uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,id,buyer_company_id),
 CONSTRAINT quotes_inquiry_fk FOREIGN KEY(tenant_id,site_id,inquiry_id) REFERENCES app.inquiries(tenant_id,site_id,id),
 CONSTRAINT quotes_company_fk FOREIGN KEY(tenant_id,buyer_company_id) REFERENCES app.buyer_companies(tenant_id,id),
 CONSTRAINT quotes_staff_fk FOREIGN KEY(tenant_id,created_by) REFERENCES app.tenant_memberships(tenant_id,user_id),
 CONSTRAINT quotes_acceptor_fk FOREIGN KEY(tenant_id,buyer_company_id,accepted_by) REFERENCES app.buyer_company_members(tenant_id,buyer_company_id,customer_id),
 CONSTRAINT quotes_total CHECK(total::numeric=subtotal::numeric+tax::numeric+shipping::numeric),
 CONSTRAINT quotes_accept_shape CHECK((status IN('accepted_pending_approval','approved','rejected'))=(accepted_by IS NOT NULL) OR (status IN('withdrawn','expired')))
);
CREATE TABLE app.quote_lines (
 tenant_id uuid NOT NULL, quote_id uuid NOT NULL,
 line_no integer NOT NULL CHECK(line_no BETWEEN 1 AND 1000),
 product_id uuid, variant_id uuid,
 description text NOT NULL CHECK(length(btrim(description)) BETWEEN 1 AND 1000),
 quantity bigint NOT NULL CHECK(quantity>0), unit_price bigint NOT NULL CHECK(unit_price>=0),
 line_total bigint NOT NULL CHECK(line_total>=0),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object' AND octet_length(snapshot::text)<=65536),
 PRIMARY KEY(tenant_id,quote_id,line_no),
 FOREIGN KEY(tenant_id,quote_id) REFERENCES app.quotes(tenant_id,id),
 FOREIGN KEY(tenant_id,product_id) REFERENCES app.products(tenant_id,id),
 FOREIGN KEY(tenant_id,product_id,variant_id) REFERENCES app.product_variants(tenant_id,product_id,id),
 CHECK(variant_id IS NULL OR product_id IS NOT NULL)
);
CREATE TABLE app.quote_approvals (
 tenant_id uuid NOT NULL, quote_id uuid NOT NULL, buyer_company_id uuid NOT NULL,
 requested_by uuid NOT NULL, decided_by uuid NOT NULL,
 decision text NOT NULL CHECK(decision IN('approved','rejected')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,quote_id),
 FOREIGN KEY(tenant_id,quote_id,buyer_company_id) REFERENCES app.quotes(tenant_id,id,buyer_company_id),
 FOREIGN KEY(tenant_id,buyer_company_id,requested_by) REFERENCES app.buyer_company_members(tenant_id,buyer_company_id,customer_id),
 FOREIGN KEY(tenant_id,buyer_company_id,decided_by) REFERENCES app.buyer_company_members(tenant_id,buyer_company_id,customer_id),
 CHECK(requested_by<>decided_by)
);
CREATE INDEX quotes_company_recent ON app.quotes(tenant_id,buyer_company_id,created_at DESC,id DESC);
-- Extend shared 011 receipts only for this module. Legacy reference-only receipts
-- and the existing ownership/fencing trigger retain their original contract.
ALTER TABLE app.idempotency_records DROP CONSTRAINT idempotency_records_actor_kind_check;
ALTER TABLE app.idempotency_records ADD CONSTRAINT idempotency_records_actor_kind_check
 CHECK(actor_kind IN('user','api_key','service','customer'));
ALTER TABLE app.idempotency_records DROP CONSTRAINT idempotency_response_shape;
ALTER TABLE app.idempotency_records ADD CONSTRAINT idempotency_response_shape CHECK(
 (state='in_progress' AND response_status IS NULL AND response_body_reference IS NULL AND completed_at IS NULL AND response_body IS NULL)
 OR (state='completed' AND response_status BETWEEN 100 AND 599 AND response_status IS NOT NULL AND completed_at IS NOT NULL AND
  ((response_body_reference IS NOT NULL AND response_body IS NULL) OR
   (operation LIKE 'quotes.%' AND response_body_reference IS NULL AND response_body IS NOT NULL
    AND jsonb_typeof(response_body)='object' AND octet_length(response_body::text)<=65536))));
DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_quotes_executor') THEN CREATE ROLE app_quotes_executor; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_quotes_executor'::regrole OR roleid='app_quotes_executor'::regrole) THEN
  RAISE EXCEPTION 'QUOTES_ROLE_MEMBERSHIP_UNSAFE'; END IF;
END $role$;
ALTER ROLE app_quotes_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_quotes_executor;
-- Default ACLs must not silently widen the new executor or tables.
DO $acl$ DECLARE r text; t text; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  FOREACH t IN ARRAY ARRAY['quotes','quote_lines','quote_approvals'] LOOP
   EXECUTE format('REVOKE ALL ON app.%I FROM %I',t,r);
  END LOOP;
 END LOOP;
 REVOKE ALL ON app.quotes,app.quote_lines,app.quote_approvals FROM PUBLIC;
END $acl$;
GRANT SELECT ON app.quotes,app.quote_lines,app.quote_approvals TO app_runtime,app_quotes_executor;
GRANT SELECT(tenant_id,site_id,id,buyer_company_id) ON app.inquiries TO app_quotes_executor;
GRANT SELECT(tenant_id,id,status) ON app.buyer_companies,app.customers TO app_quotes_executor;
GRANT SELECT(tenant_id,buyer_company_id,customer_id,role_code,status) ON app.buyer_company_members TO app_quotes_executor;
GRANT SELECT(id,status) ON app.users,app.tenants TO app_quotes_executor;
GRANT SELECT(tenant_id,user_id,role,status) ON app.tenant_memberships TO app_quotes_executor;
GRANT INSERT(tenant_id,id,site_id,inquiry_id,buyer_company_id,created_by,currency,subtotal,tax,shipping,total,snapshot,expires_at) ON app.quotes TO app_quotes_executor;
GRANT UPDATE(status,version,accepted_by,updated_at) ON app.quotes TO app_quotes_executor;
GRANT INSERT(tenant_id,quote_id,line_no,product_id,variant_id,description,quantity,unit_price,line_total,snapshot) ON app.quote_lines TO app_quotes_executor;
GRANT INSERT(tenant_id,quote_id,buyer_company_id,requested_by,decided_by,decision) ON app.quote_approvals TO app_quotes_executor;
GRANT INSERT(tenant_id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events TO app_quotes_executor;
DO $rls$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['quotes','quote_lines','quote_approvals'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_quotes_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['inquiries','buyer_companies','buyer_company_members','customers','tenant_memberships','outbox_events'] LOOP
  EXECUTE format('CREATE POLICY quotes_executor_scope ON app.%I TO app_quotes_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',t);
 END LOOP;
END $rls$;
CREATE FUNCTION app.quote_actor_access(p_tenant uuid,p_company uuid,p_kind text,p_actor uuid,p_operation text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT coalesce(p_tenant=nullif(current_setting('app.tenant_id',true),'')::uuid AND p_actor IS NOT NULL
 AND EXISTS(SELECT FROM app.tenants t WHERE t.id=p_tenant AND t.status='active')
 AND EXISTS(SELECT FROM app.buyer_companies c WHERE c.tenant_id=p_tenant AND c.id=p_company AND c.status='active')
 AND ((p_kind='staff' AND p_operation IN('create','read','send','withdraw','expire') AND EXISTS(
  SELECT FROM app.tenant_memberships m JOIN app.users u ON u.id=m.user_id
  WHERE m.tenant_id=p_tenant AND m.user_id=p_actor AND m.status='active' AND u.status='active' AND (p_operation='read' OR m.role IN('owner','admin','catalog_editor'))))
 OR (p_kind='customer' AND p_operation IN('read','accept','approve','reject') AND EXISTS(
  SELECT FROM app.buyer_company_members m JOIN app.customers c ON(c.tenant_id,c.id)=(m.tenant_id,m.customer_id)
  WHERE m.tenant_id=p_tenant AND m.buyer_company_id=p_company AND m.customer_id=p_actor
  AND m.status='active' AND c.status='active'
  AND (p_operation IN('read','accept') OR m.role_code IN('approver','admin'))))),false);
$$;
CREATE FUNCTION app.guard_quote() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'QUOTES_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.status<>'draft' OR NEW.version<>1 OR NEW.accepted_by IS NOT NULL THEN
   RAISE EXCEPTION 'QUOTES_INITIAL_STATE' USING ERRCODE='23514'; END IF;
 ELSE
  IF ROW(NEW.tenant_id,NEW.id,NEW.site_id,NEW.inquiry_id,NEW.buyer_company_id,NEW.created_by,NEW.currency,NEW.subtotal,NEW.tax,NEW.shipping,NEW.total,NEW.snapshot,NEW.expires_at,NEW.created_at)
   IS DISTINCT FROM ROW(OLD.tenant_id,OLD.id,OLD.site_id,OLD.inquiry_id,OLD.buyer_company_id,OLD.created_by,OLD.currency,OLD.subtotal,OLD.tax,OLD.shipping,OLD.total,OLD.snapshot,OLD.expires_at,OLD.created_at) THEN
   RAISE EXCEPTION 'QUOTES_IMMUTABLE' USING ERRCODE='55000'; END IF;
  IF NOT ((OLD.status='draft' AND NEW.status='sent') OR (OLD.status='sent' AND NEW.status='accepted_pending_approval')
   OR(OLD.status='accepted_pending_approval' AND NEW.status IN('approved','rejected'))
   OR(OLD.status IN('sent','accepted_pending_approval') AND NEW.status IN('withdrawn','expired')))
   OR NEW.version<>OLD.version+1 THEN RAISE EXCEPTION 'QUOTES_STATE_VERSION' USING ERRCODE='23514'; END IF;
  IF NEW.status IN('sent','accepted_pending_approval','approved','rejected') AND NEW.expires_at<=clock_timestamp() THEN
   RAISE EXCEPTION 'QUOTES_EXPIRED' USING ERRCODE='22023'; END IF;
  IF NEW.status='expired' AND NEW.expires_at>clock_timestamp() THEN RAISE EXCEPTION 'QUOTES_NOT_EXPIRED' USING ERRCODE='22023'; END IF;
  IF NEW.status='accepted_pending_approval' THEN
   IF NEW.accepted_by IS NULL OR OLD.accepted_by IS NOT NULL THEN RAISE EXCEPTION 'QUOTES_ACCEPTOR' USING ERRCODE='23514'; END IF;
  ELSIF NEW.accepted_by IS DISTINCT FROM OLD.accepted_by THEN RAISE EXCEPTION 'QUOTES_ACCEPTOR_IMMUTABLE' USING ERRCODE='55000'; END IF;
 END IF;
 -- Runtime has no direct mutation privilege; even a future grant must not turn
 -- transaction-local GUCs into authority to choose a lifecycle or actor.
 IF current_user<>'app_quotes_executor' THEN RAISE EXCEPTION 'QUOTES_COMMAND_REQUIRED' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER quote_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quotes FOR EACH ROW EXECUTE FUNCTION app.guard_quote();
CREATE FUNCTION app.guard_quote_child() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'QUOTES_CHILD_IMMUTABLE' USING ERRCODE='55000'; END IF;
 IF current_user<>'app_quotes_executor' THEN RAISE EXCEPTION 'QUOTES_COMMAND_REQUIRED' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER quote_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quote_lines FOR EACH ROW EXECUTE FUNCTION app.guard_quote_child();
CREATE TRIGGER quote_approvals_guard BEFORE INSERT OR UPDATE OR DELETE ON app.quote_approvals FOR EACH ROW EXECUTE FUNCTION app.guard_quote_child();
CREATE FUNCTION app.quote_outbox() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key)
 VALUES(NEW.tenant_id,CASE TG_OP WHEN 'INSERT' THEN 'quotes.created' ELSE 'quotes.'||NEW.status END,
  jsonb_build_object('quote_id',NEW.id,'status',NEW.status,'version',NEW.version),
  'quotes',NEW.id::text||':'||NEW.version::text);
 RETURN NEW;
END $$;
CREATE TRIGGER quote_outbox AFTER INSERT OR UPDATE OF status ON app.quotes FOR EACH ROW EXECUTE FUNCTION app.quote_outbox();
CREATE FUNCTION app.create_quote(p_tenant uuid,p_site uuid,p_inquiry uuid,p_company uuid,p_actor uuid,p_currency text,
 p_subtotal bigint,p_tax bigint,p_shipping bigint,p_total bigint,p_snapshot jsonb,p_expires timestamptz,p_lines jsonb)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE q uuid; n integer; line_sum numeric; BEGIN
 IF NOT app.quote_actor_access(p_tenant,p_company,'staff',p_actor,'create') THEN RAISE EXCEPTION 'QUOTES_ACTOR' USING ERRCODE='42501'; END IF;
 IF p_expires IS NULL OR NOT isfinite(p_expires) OR p_expires<=clock_timestamp()
 OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR jsonb_array_length(p_lines) NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'QUOTES_CREATE_ARGUMENT' USING ERRCODE='22023'; END IF;
 IF EXISTS(SELECT FROM app.inquiries i WHERE(i.tenant_id,i.site_id,i.id)=(p_tenant,p_site,p_inquiry)
  AND i.buyer_company_id IS NOT NULL AND i.buyer_company_id<>p_company) THEN RAISE EXCEPTION 'QUOTES_INQUIRY_COMPANY' USING ERRCODE='23503'; END IF;
 INSERT INTO app.quotes(tenant_id,site_id,inquiry_id,buyer_company_id,created_by,currency,subtotal,tax,shipping,total,snapshot,expires_at)
 VALUES(p_tenant,p_site,p_inquiry,p_company,p_actor,p_currency,p_subtotal,p_tax,p_shipping,p_total,p_snapshot,p_expires) RETURNING id INTO q;
 INSERT INTO app.quote_lines(tenant_id,quote_id,line_no,product_id,variant_id,description,quantity,unit_price,line_total,snapshot)
 SELECT p_tenant,q,l.line_no,l.product_id,l.variant_id,l.description,l.quantity,l.unit_price,l.line_total,l.snapshot
 FROM jsonb_to_recordset(p_lines) AS l(line_no integer,product_id uuid,variant_id uuid,description text,quantity bigint,unit_price bigint,line_total bigint,snapshot jsonb);
 SELECT count(*),sum(line_total::numeric) INTO n,line_sum FROM app.quote_lines WHERE tenant_id=p_tenant AND quote_id=q;
 IF n<>jsonb_array_length(p_lines) OR line_sum<>p_subtotal::numeric THEN RAISE EXCEPTION 'QUOTES_LINE_TOTAL' USING ERRCODE='23514'; END IF;
 RETURN q;
END $$;
CREATE FUNCTION app.transition_quote(p_tenant uuid,p_quote uuid,p_version bigint,p_action text,p_kind text,p_actor uuid)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE q app.quotes; next_state text; BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN RAISE EXCEPTION 'QUOTES_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 SELECT * INTO q FROM app.quotes WHERE tenant_id=p_tenant AND id=p_quote FOR UPDATE;
 IF NOT FOUND OR NOT app.quote_actor_access(p_tenant,q.buyer_company_id,p_kind,p_actor,'read') THEN RAISE EXCEPTION 'QUOTES_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF NOT app.quote_actor_access(p_tenant,q.buyer_company_id,p_kind,p_actor,p_action) THEN RAISE EXCEPTION 'QUOTES_ACTOR' USING ERRCODE='42501'; END IF;
 IF p_version IS NULL OR q.version<>p_version THEN RAISE EXCEPTION 'QUOTES_VERSION' USING ERRCODE='P0003'; END IF;
 next_state:=CASE p_action WHEN 'send' THEN 'sent' WHEN 'accept' THEN 'accepted_pending_approval'
  WHEN 'approve' THEN 'approved' WHEN 'reject' THEN 'rejected' WHEN 'withdraw' THEN 'withdrawn' WHEN 'expire' THEN 'expired' END;
 IF next_state IS NULL THEN RAISE EXCEPTION 'QUOTES_ACTION' USING ERRCODE='22023'; END IF;
 IF p_action IN('approve','reject') THEN
  IF q.status<>'accepted_pending_approval' THEN RAISE EXCEPTION 'QUOTES_STATE_VERSION' USING ERRCODE='23514'; END IF;
  IF q.accepted_by=p_actor THEN RAISE EXCEPTION 'QUOTES_SELF_APPROVAL' USING ERRCODE='42501'; END IF;
  IF q.expires_at<=clock_timestamp() THEN RAISE EXCEPTION 'QUOTES_EXPIRED' USING ERRCODE='22023'; END IF;
  INSERT INTO app.quote_approvals(tenant_id,quote_id,buyer_company_id,requested_by,decided_by,decision)
  VALUES(p_tenant,p_quote,q.buyer_company_id,q.accepted_by,p_actor,next_state);
 END IF;
 UPDATE app.quotes SET status=next_state,version=version+1,
  accepted_by=CASE WHEN p_action='accept' THEN p_actor ELSE accepted_by END,updated_at=clock_timestamp()
 WHERE tenant_id=p_tenant AND id=p_quote RETURNING version INTO p_version;
 RETURN p_version;
END $$;
-- New function bodies are owned by a capability that cannot log in or be SET ROLE'd.
GRANT CREATE ON SCHEMA app TO app_quotes_executor;
ALTER FUNCTION app.quote_actor_access(uuid,uuid,text,uuid,text) OWNER TO app_quotes_executor;
ALTER FUNCTION app.guard_quote() OWNER TO app_quotes_executor;
ALTER FUNCTION app.guard_quote_child() OWNER TO app_quotes_executor;
ALTER FUNCTION app.quote_outbox() OWNER TO app_quotes_executor;
ALTER FUNCTION app.create_quote(uuid,uuid,uuid,uuid,uuid,text,bigint,bigint,bigint,bigint,jsonb,timestamptz,jsonb) OWNER TO app_quotes_executor;
ALTER FUNCTION app.transition_quote(uuid,uuid,bigint,text,text,uuid) OWNER TO app_quotes_executor;
REVOKE CREATE ON SCHEMA app FROM app_quotes_executor;
DO $routine_acl$ DECLARE f record; r text; BEGIN
 FOR f IN SELECT p.oid::regprocedure AS sig FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND p.proname IN('quote_actor_access','guard_quote','guard_quote_child','quote_outbox','create_quote','transition_quote') LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.sig);
  FOR r IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') AND rolname<>'app_quotes_executor' LOOP
   EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f.sig,r);
  END LOOP;
 END LOOP;
END $routine_acl$;
GRANT EXECUTE ON FUNCTION app.quote_actor_access(uuid,uuid,text,uuid,text),
 app.create_quote(uuid,uuid,uuid,uuid,uuid,text,bigint,bigint,bigint,bigint,jsonb,timestamptz,jsonb),
 app.transition_quote(uuid,uuid,bigint,text,text,uuid) TO app_runtime;
