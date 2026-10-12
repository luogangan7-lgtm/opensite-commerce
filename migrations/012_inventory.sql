-- C-11a / Inventory owns these four tables; depends on tenants and catalog SKUs.
-- Called by the forward-only migrator. Application callers use public routines.
-- Invariants: tenant-scoped integer stock, exactly-once reservation transitions,
-- append-only matching movements, no direct runtime writes or executor membership.
DO $roles$
DECLARE role_name text; member_name text;
BEGIN
 FOREACH role_name IN ARRAY ARRAY['app_inventory_manager','app_inventory_executor'] LOOP
  IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('CREATE ROLE %I NOLOGIN',role_name);
  END IF;
  EXECUTE format('ALTER ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT',role_name);
  FOR member_name IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid
   WHERE m.member=role_name::regrole LOOP
   EXECUTE format('REVOKE %I FROM %I CASCADE',member_name,role_name);
  END LOOP;
 END LOOP;
 FOR member_name IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member
  WHERE m.roleid='app_inventory_executor'::regrole LOOP
  EXECUTE format('REVOKE app_inventory_executor FROM %I CASCADE',member_name);
 END LOOP;
END $roles$;
GRANT USAGE ON SCHEMA app TO app_inventory_manager,app_inventory_executor;
REVOKE CREATE ON SCHEMA app FROM app_inventory_manager,app_inventory_executor;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_inventory_manager,app_inventory_executor;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM app_inventory_manager,app_inventory_executor;
DO $old_columns$
DECLARE t record; columns text; member_name text;
BEGIN
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN('r','p') LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO columns
  FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE SELECT(%s),INSERT(%s),UPDATE(%s),REFERENCES(%s) ON app.%I FROM app_inventory_manager,app_inventory_executor',columns,columns,columns,columns,t.relname);
 END LOOP;
 -- Private login membership may be provisioned outside the repo, but another
 -- internal NOLOGIN capability must not acquire the manager by SET ROLE.
 FOR member_name IN SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member
  WHERE m.roleid='app_inventory_manager'::regrole AND NOT r.rolcanlogin LOOP
  EXECUTE format('REVOKE app_inventory_manager FROM %I CASCADE',member_name);
 END LOOP;
END $old_columns$;

CREATE TABLE app.warehouses(
 tenant_id uuid NOT NULL REFERENCES app.tenants(id), id uuid NOT NULL DEFAULT uuidv7(),
 name text NOT NULL DEFAULT 'Default warehouse' CHECK(btrim(name)<>''),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id) -- ponytail: MVP single warehouse; future forward migration lifts this.
);
CREATE TABLE app.stock_levels(
 tenant_id uuid NOT NULL, warehouse_id uuid NOT NULL, sku_id uuid NOT NULL,
 on_hand bigint NOT NULL DEFAULT 0 CHECK(on_hand>=0),
 reserved bigint NOT NULL DEFAULT 0,
 CONSTRAINT inventory_stock_bounds CHECK(reserved>=0 AND reserved<=on_hand),
 PRIMARY KEY(tenant_id,warehouse_id,sku_id),
 FOREIGN KEY(tenant_id,warehouse_id) REFERENCES app.warehouses(tenant_id,id),
 FOREIGN KEY(tenant_id,sku_id) REFERENCES app.product_variants(tenant_id,id)
);
CREATE TABLE app.inventory_reservations(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
 warehouse_id uuid NOT NULL, sku_id uuid NOT NULL, quantity bigint NOT NULL CHECK(quantity>0),
 owner_type text NOT NULL CHECK(btrim(owner_type)<>'' AND length(owner_type)<=64), owner_id uuid NOT NULL,
 idempotency_key text NOT NULL CHECK(btrim(idempotency_key)<>'' AND length(idempotency_key)<=128),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 ttl_seconds integer NOT NULL CHECK(ttl_seconds>0),
 state text NOT NULL DEFAULT 'active' CHECK(state IN('active','committed','released','expired')),
 expires_at timestamptz NOT NULL CHECK(isfinite(expires_at)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), finished_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,idempotency_key),
 UNIQUE(tenant_id,id,warehouse_id,sku_id),
 FOREIGN KEY(tenant_id,warehouse_id,sku_id) REFERENCES app.stock_levels(tenant_id,warehouse_id,sku_id),
 CHECK((state='active' AND finished_at IS NULL) OR (state<>'active' AND finished_at IS NOT NULL))
);
CREATE INDEX inventory_due ON app.inventory_reservations(tenant_id,warehouse_id,sku_id,id)
 WHERE state='active';
-- Expiry sweep finds due rows by time (O(due), not O(active)); inventory_due keeps lock order.
CREATE INDEX inventory_expiry ON app.inventory_reservations(tenant_id,expires_at)
 WHERE state='active';
CREATE TABLE app.inventory_movements(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(), warehouse_id uuid NOT NULL, sku_id uuid NOT NULL,
 reservation_id uuid, owner_type text NOT NULL CHECK(btrim(owner_type)<>''), owner_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN('inbound','outbound','adjustment','reserve','release','commit','expire')),
 quantity bigint NOT NULL CHECK(quantity>0), on_hand_delta bigint NOT NULL, reserved_delta bigint NOT NULL,
 on_hand_after bigint NOT NULL CHECK(on_hand_after>=0), reserved_after bigint NOT NULL CHECK(reserved_after>=0 AND reserved_after<=on_hand_after),
 reason text NOT NULL CHECK(btrim(reason)<>'' AND length(reason)<=1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id),
 FOREIGN KEY(tenant_id,warehouse_id,sku_id) REFERENCES app.stock_levels(tenant_id,warehouse_id,sku_id),
 FOREIGN KEY(tenant_id,reservation_id,warehouse_id,sku_id)
  REFERENCES app.inventory_reservations(tenant_id,id,warehouse_id,sku_id),
 CHECK((kind='inbound' AND reservation_id IS NULL AND reserved_delta=0 AND on_hand_delta=quantity)
    OR (kind='outbound' AND reservation_id IS NULL AND reserved_delta=0 AND on_hand_delta=-quantity)
    OR (kind='adjustment' AND reservation_id IS NULL AND reserved_delta=0 AND (on_hand_delta=quantity OR on_hand_delta=-quantity))
    OR (kind='reserve' AND reservation_id IS NOT NULL AND on_hand_delta=0 AND reserved_delta=quantity)
    OR (kind IN('release','expire') AND reservation_id IS NOT NULL AND on_hand_delta=0 AND reserved_delta=-quantity)
    OR (kind='commit' AND reservation_id IS NOT NULL AND on_hand_delta=-quantity AND reserved_delta=-quantity))
);

DO $policies$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['warehouses','stock_levels','inventory_reservations','inventory_movements'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',table_name);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_inventory_manager,app_inventory_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',table_name);
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC,app_runtime,app_inventory_manager,app_inventory_executor',table_name);
  EXECUTE format('GRANT SELECT ON app.%I TO app_runtime,app_inventory_manager,app_inventory_executor',table_name);
 END LOOP;
END $policies$;
GRANT SELECT ON app.tenants TO app_inventory_executor;
GRANT INSERT ON app.warehouses,app.stock_levels,app.inventory_reservations,app.inventory_movements TO app_inventory_executor;
GRANT UPDATE ON app.stock_levels,app.inventory_reservations TO app_inventory_executor;

CREATE FUNCTION app.inventory_context() RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid;
BEGIN
 BEGIN t:=nullif(current_setting('app.tenant_id',true),'')::uuid;
 EXCEPTION WHEN invalid_text_representation THEN
  RAISE EXCEPTION 'INVENTORY_TENANT_CONTEXT_REQUIRED' USING ERRCODE='42501';
 END;
 IF t IS NULL OR NOT EXISTS(SELECT FROM app.tenants WHERE id=t AND status='active') THEN
  RAISE EXCEPTION 'INVENTORY_TENANT_CONTEXT_REQUIRED' USING ERRCODE='42501';
 END IF;
 RETURN t;
END $fn$;

CREATE FUNCTION app.inventory_immutable_movement() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN RAISE EXCEPTION 'INVENTORY_MOVEMENT_IMMUTABLE' USING ERRCODE='55000'; END $fn$;
CREATE TRIGGER inventory_movements_immutable BEFORE UPDATE OR DELETE ON app.inventory_movements
FOR EACH ROW EXECUTE FUNCTION app.inventory_immutable_movement();

-- Trusted inventory manager only: stock receipt/dispatch/correction, not a buyer capability.
CREATE FUNCTION app.inventory_adjust(p_sku uuid,p_delta bigint,p_reason text,p_reference uuid,p_kind text DEFAULT 'adjustment')
RETURNS app.stock_levels LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); w uuid; s app.stock_levels;
BEGIN
 IF p_sku IS NULL OR p_delta IS NULL OR p_delta=0 OR p_reference IS NULL
  OR p_reason IS NULL OR btrim(p_reason)='' OR length(p_reason)>1000
  OR p_kind IS NULL OR p_kind NOT IN('inbound','outbound','adjustment')
  OR (p_kind='inbound' AND p_delta<0) OR (p_kind='outbound' AND p_delta>0) THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 INSERT INTO app.warehouses(tenant_id) VALUES(t) ON CONFLICT(tenant_id) DO NOTHING;
 SELECT id INTO STRICT w FROM app.warehouses WHERE tenant_id=t;
 INSERT INTO app.stock_levels(tenant_id,warehouse_id,sku_id) VALUES(t,w,p_sku)
  ON CONFLICT(tenant_id,warehouse_id,sku_id) DO NOTHING;
 SELECT * INTO STRICT s FROM app.stock_levels WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku FOR UPDATE;
 IF p_delta<0 AND p_delta < s.reserved-s.on_hand THEN
  RAISE EXCEPTION 'INVENTORY_INSUFFICIENT_STOCK' USING ERRCODE='P0001';
 END IF;
 UPDATE app.stock_levels SET on_hand=on_hand+p_delta WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku RETURNING * INTO s;
 INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,owner_type,owner_id,kind,quantity,
  on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
 VALUES(t,w,p_sku,'stock_adjustment',p_reference,p_kind,abs(p_delta),p_delta,0,s.on_hand,s.reserved,p_reason);
 RETURN s;
END $fn$;

CREATE FUNCTION app.inventory_reserve(p_sku uuid,p_quantity bigint,p_owner_type text,p_owner_id uuid,p_key text,p_ttl_seconds integer DEFAULT 900)
RETURNS app.inventory_reservations LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); w uuid; r app.inventory_reservations; s app.stock_levels; h bytea;
BEGIN
 IF p_sku IS NULL OR p_quantity IS NULL OR p_quantity<=0 OR p_owner_id IS NULL
  OR p_owner_type IS NULL OR btrim(p_owner_type)='' OR length(p_owner_type)>64
  OR p_key IS NULL OR btrim(p_key)='' OR length(p_key)>128 OR p_ttl_seconds IS NULL OR p_ttl_seconds<=0 THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 h:=sha256(convert_to(jsonb_build_object('sku',p_sku,'quantity',p_quantity,'owner_type',p_owner_type,
  'owner_id',p_owner_id,'ttl_seconds',p_ttl_seconds)::text,'UTF8'));
 -- Serialize the key BEFORE stock; replay never locks an already-existing reservation.
 PERFORM pg_advisory_xact_lock(hashtextextended(t::text||':'||p_key,0));
 SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND idempotency_key=p_key;
 IF FOUND THEN
  IF r.request_hash<>h THEN RAISE EXCEPTION 'INVENTORY_IDEMPOTENCY_KEY_REUSED' USING ERRCODE='22023'; END IF;
  RETURN r;
 END IF;
 SELECT id INTO w FROM app.warehouses WHERE tenant_id=t;
 SELECT * INTO s FROM app.stock_levels WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'INVENTORY_STOCK_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF s.on_hand-s.reserved<p_quantity THEN
  RAISE EXCEPTION 'INVENTORY_INSUFFICIENT_STOCK' USING ERRCODE='P0001';
 END IF;
 UPDATE app.stock_levels SET reserved=reserved+p_quantity WHERE tenant_id=t AND warehouse_id=w AND sku_id=p_sku RETURNING * INTO s;
 INSERT INTO app.inventory_reservations(tenant_id,warehouse_id,sku_id,quantity,owner_type,owner_id,idempotency_key,request_hash,ttl_seconds,expires_at)
 VALUES(t,w,p_sku,p_quantity,p_owner_type,p_owner_id,p_key,h,p_ttl_seconds,clock_timestamp()+make_interval(secs=>p_ttl_seconds)) RETURNING * INTO r;
 INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,reservation_id,owner_type,owner_id,kind,quantity,
  on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
 VALUES(t,w,p_sku,r.id,p_owner_type,p_owner_id,'reserve',p_quantity,0,p_quantity,s.on_hand,s.reserved,'reservation created');
 RETURN r;
END $fn$;

-- Closed implementation helper: res -> stock lock order for every terminal transition.
CREATE FUNCTION app.inventory_finish(p_id uuid,p_target text) RETURNS app.inventory_reservations
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); r app.inventory_reservations; s app.stock_levels; d bigint;
BEGIN
 IF p_id IS NULL OR p_target IS NULL OR p_target NOT IN('released','committed','expired') THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_ARGUMENT' USING ERRCODE='22023';
 END IF;
 SELECT * INTO r FROM app.inventory_reservations WHERE tenant_id=t AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'INVENTORY_RESERVATION_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.state=p_target THEN RETURN r; END IF;
 IF r.state<>'active' THEN RAISE EXCEPTION 'INVENTORY_INVALID_TRANSITION' USING ERRCODE='55000'; END IF;
 SELECT * INTO STRICT s FROM app.stock_levels
 WHERE tenant_id=t AND warehouse_id=r.warehouse_id AND sku_id=r.sku_id FOR UPDATE;
 -- Wall clock AFTER both waits, not transaction-start now().
 IF p_target='committed' AND r.expires_at<=clock_timestamp() THEN
  RAISE EXCEPTION 'INVENTORY_RESERVATION_EXPIRED' USING ERRCODE='55000';
 END IF;
 IF p_target='expired' AND r.expires_at>clock_timestamp() THEN RETURN NULL; END IF;
 d:=CASE WHEN p_target='committed' THEN -r.quantity ELSE 0 END;
 UPDATE app.stock_levels SET on_hand=on_hand+d,reserved=reserved-r.quantity
 WHERE tenant_id=t AND warehouse_id=r.warehouse_id AND sku_id=r.sku_id RETURNING * INTO s;
 UPDATE app.inventory_reservations SET state=p_target,finished_at=clock_timestamp()
 WHERE tenant_id=t AND id=p_id RETURNING * INTO r;
 INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,reservation_id,owner_type,owner_id,kind,quantity,
  on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
 VALUES(t,r.warehouse_id,r.sku_id,r.id,r.owner_type,r.owner_id,
  CASE p_target WHEN 'committed' THEN 'commit' WHEN 'expired' THEN 'expire' ELSE 'release' END,
  r.quantity,d,-r.quantity,s.on_hand,s.reserved,'reservation '||p_target);
 RETURN r;
END $fn$;
CREATE FUNCTION app.inventory_release(p_id uuid) RETURNS app.inventory_reservations
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish(p_id,''released'')';
CREATE FUNCTION app.inventory_commit(p_id uuid) RETURNS app.inventory_reservations
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp
AS 'SELECT app.inventory_finish(p_id,''committed'')';
CREATE FUNCTION app.inventory_expire_due(p_batch integer DEFAULT 100) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
DECLARE t uuid:=app.inventory_context(); r record; finished app.inventory_reservations; n integer:=0; cutoff timestamptz;
BEGIN
 IF p_batch IS NULL OR p_batch<1 OR p_batch>1000 THEN
  RAISE EXCEPTION 'INVENTORY_INVALID_BATCH' USING ERRCODE='22023';
 END IF;
 cutoff:=clock_timestamp();  -- a parameter, not a volatile call, so inventory_expiry is usable
 -- Pick due candidates by time (O(due)), then lock them in deterministic stock order.
 FOR r IN SELECT id FROM app.inventory_reservations WHERE tenant_id=t AND state='active' AND expires_at<=cutoff
  AND id IN (SELECT id FROM app.inventory_reservations WHERE tenant_id=t AND state='active' AND expires_at<=cutoff
             ORDER BY expires_at LIMIT p_batch)
  ORDER BY warehouse_id,sku_id,id FOR UPDATE SKIP LOCKED LOOP
  finished:=app.inventory_finish(r.id,'expired');
  IF finished.id IS NOT NULL THEN n:=n+1; END IF;
 END LOOP;
 RETURN n;
END $fn$;
DO $function_owners$
DECLARE f record;
BEGIN
 FOR f IN SELECT p.oid::regprocedure AS identity FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='app' AND p.proname=ANY(ARRAY['inventory_context','inventory_immutable_movement',
  'inventory_adjust','inventory_reserve','inventory_finish','inventory_release','inventory_commit','inventory_expire_due']) LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_inventory_executor',f.identity);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC,app_runtime,app_inventory_manager',f.identity);
 END LOOP;
END $function_owners$;
GRANT EXECUTE ON FUNCTION app.inventory_adjust(uuid,bigint,text,uuid,text) TO app_inventory_manager;
GRANT EXECUTE ON FUNCTION app.inventory_reserve(uuid,bigint,text,uuid,text,integer),
 app.inventory_release(uuid),app.inventory_commit(uuid),app.inventory_expire_due(integer) TO app_runtime;
