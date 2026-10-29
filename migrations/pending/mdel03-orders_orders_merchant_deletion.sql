-- stable-id: mdel03-orders
-- depends-on: mdel02-identity, c19d2-projections, b-privacy-hardening
-- module: orders
-- Responsibility: Orders obligation witness, lifecycle prefixes and exact scrubs.
-- Dependencies: Tenant fence; existing Orders read authority over quantity facts.
-- Callers: Orders commands and dormant DataRights owner ports. No activation.
-- Invariants: fence -> sorted orders -> local leaves; financial cells unchanged.
-- Rollback: revert unlanded code; after landing use a new forward migration.
DO $role$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_orders_privacy_executor') THEN
  CREATE ROLE app_orders_privacy_executor NOLOGIN;
 END IF;
 ALTER ROLE app_orders_privacy_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
 IF EXISTS(SELECT FROM pg_auth_members WHERE roleid='app_orders_privacy_executor'::regrole OR member='app_orders_privacy_executor'::regrole) THEN
  RAISE EXCEPTION 'Orders privacy role must be unassumable'; END IF;
END $role$;
GRANT USAGE ON SCHEMA app TO app_orders_privacy_executor;
GRANT SELECT ON app.orders,app.order_lines,app.order_adjustments TO app_orders_privacy_executor;
GRANT UPDATE(guest_contact,shipping_snapshot) ON app.orders TO app_orders_privacy_executor;
GRANT UPDATE(sku,tax_snapshot) ON app.order_lines TO app_orders_privacy_executor;
GRANT UPDATE(reason) ON app.order_adjustments TO app_orders_privacy_executor;
GRANT SELECT(tenant_id,id,request_id,epoch,state,phase) ON app.tenant_lifecycles TO app_orders_privacy_executor;
CREATE POLICY orders_privacy_lifecycle ON app.tenant_lifecycles FOR SELECT TO app_orders_privacy_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
DO $policies$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['orders','order_lines','order_adjustments'] LOOP
  EXECUTE format('CREATE POLICY orders_merchant_scrub ON app.%I TO app_orders_privacy_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',t);
 END LOOP;
END $policies$;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_read_fence(uuid) TO app_orders_executor,app_orders_privacy_executor;
GRANT EXECUTE ON FUNCTION app.tenant_lifecycle_write_fence(uuid),app.tenant_lifecycle_drain_fence(uuid,bigint) TO app_orders_executor;

CREATE FUNCTION app.orders_merchant_mutation_fence(p_drain boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; e bigint;
BEGIN
 IF p_drain IS TRUE THEN
  e:=app.tenant_lifecycle_read_fence(t);
  PERFORM app.tenant_lifecycle_drain_fence(t,e);
 ELSE
  PERFORM app.tenant_lifecycle_write_fence(t);
 END IF;
END $$;
-- Add the common prefix at the existing context boundary, before every module
-- lock, including legacy/SQL/financial callers. Preserve exact OID/owner/ACL.
DO $prefix$ DECLARE f record; revised text; prefix text; BEGIN
 FOR f IN SELECT p.oid::regprocedure signature,p.proname,p.prosrc,pg_get_functiondef(p.oid) def
  FROM pg_proc p WHERE p.pronamespace='app'::regnamespace AND p.proname IN(
   'orders_context','orders_create','orders_check_creation','orders_lock_reservations',
   'orders_transition','orders_fulfillment_fence','orders_fulfillment_advance',
   'orders_refund_budget','orders_finish_return_refund','orders_compensate_refund') LOOP
  prefix:=CASE f.proname
   WHEN 'orders_context' THEN 'PERFORM app.tenant_lifecycle_read_fence(t); RETURN t;'
   WHEN 'orders_transition' THEN 'PERFORM app.orders_merchant_mutation_fence(p_target IN(''paid'',''cancelled''));'
   WHEN 'orders_refund_budget' THEN 'PERFORM app.orders_merchant_mutation_fence(p_phase IN(''freeze'',''lock'',''fail'',''complete''));'
   WHEN 'orders_finish_return_refund' THEN 'PERFORM app.orders_merchant_mutation_fence(true);'
   WHEN 'orders_compensate_refund' THEN 'PERFORM app.orders_merchant_mutation_fence(true);'
   ELSE 'PERFORM app.orders_merchant_mutation_fence(false);' END;
  IF f.proname='orders_context' THEN
   revised:=replace(f.prosrc,'RETURN t;',prefix);
  ELSE
   revised:=regexp_replace(f.prosrc,'\mBEGIN\M','BEGIN '||prefix);
  END IF;
  IF revised=f.prosrc THEN RAISE EXCEPTION 'Orders prefix anchor missing %',f.signature; END IF;
  EXECUTE replace(f.def,f.prosrc,revised);
 END LOOP;
END $prefix$;

CREATE FUNCTION app.orders_merchant_obligations(p_tenant uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE result jsonb;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 WITH blocked AS MATERIALIZED(
  SELECT o.id FROM app.orders o WHERE o.tenant_id=p_tenant AND o.sealed
   AND o.state IN('paid','partially_fulfilled','fulfilled','partially_refunded','refunded')
   AND EXISTS(SELECT FROM app.order_lines l WHERE l.tenant_id=p_tenant AND l.order_id=o.id
    AND l.quantity::numeric >
     coalesce((SELECT sum(s.quantity::numeric) FROM app.shipment_lines s WHERE s.tenant_id=p_tenant AND s.order_line_id=l.id),0)
     +coalesce((SELECT sum(rl.quantity::numeric) FROM app.return_lines rl JOIN app.returns r
       ON(r.tenant_id,r.id)=(rl.tenant_id,rl.return_id)
       WHERE rl.tenant_id=p_tenant AND rl.order_line_id=l.id AND r.kind='unshipped_cancel' AND r.sealed),0)))
 SELECT jsonb_build_object('count',(SELECT pg_catalog.count(*) FROM blocked),'object_ids',
  ARRAY(SELECT b.id FROM blocked b ORDER BY b.id LIMIT 16)) INTO result;
 RETURN result;
END $$;

-- A separate invoker guard admits ONLY registry replacements. Existing guards
-- keep their complete bodies, with a narrowly scoped pass-through to this guard.
CREATE FUNCTION app.orders_merchant_scrub_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE replacement jsonb;
BEGIN
 IF current_user<>'app_orders_privacy_executor' THEN
  IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
 END IF;
 IF TG_TABLE_NAME='orders' THEN replacement:= jsonb_build_object('guest_contact',CASE WHEN OLD.guest_contact IS NULL THEN NULL
   ELSE jsonb_build_object('email','erased-'||OLD.id::text||'@invalid.test','erased',true) END,
   'shipping_snapshot',jsonb_build_object('erased',true,'amount_minor',OLD.shipping_minor,'currency',OLD.currency));
 ELSIF TG_TABLE_NAME='order_lines' THEN replacement:= jsonb_build_object('sku','[erased]','tax_snapshot',jsonb_build_object('erased',true));
 ELSIF TG_TABLE_NAME='order_adjustments' THEN replacement:= jsonb_build_object('reason','[erased]'); END IF;
 IF TG_OP<>'UPDATE' OR replacement IS NULL OR to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD)||replacement THEN
  RAISE EXCEPTION 'MDEL03_EXACT_SCRUB_ONLY' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
DO $guards$ DECLARE f record; t text; BEGIN
 FOR f IN SELECT p.prosrc,pg_get_functiondef(p.oid) def FROM pg_proc p
  WHERE p.pronamespace='app'::regnamespace AND p.proname IN('orders_guard','data_rights_line_guard') LOOP
  EXECUTE replace(f.def,f.prosrc,regexp_replace(f.prosrc,'\mBEGIN\M',
   'BEGIN IF current_user=''app_orders_privacy_executor'' THEN RETURN NEW; END IF;'));
 END LOOP;
 FOREACH t IN ARRAY ARRAY['orders','order_lines','order_adjustments'] LOOP
  EXECUTE format('CREATE TRIGGER orders_merchant_exact_scrub BEFORE UPDATE OR DELETE ON app.%I FOR EACH ROW EXECUTE FUNCTION app.orders_merchant_scrub_guard()',t);
 END LOOP;
END $guards$;
DROP TRIGGER order_adjustments_snapshot_guard ON app.order_adjustments;
CREATE TRIGGER order_adjustments_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON app.order_adjustments
 FOR EACH ROW WHEN(current_user NOT IN('app_data_rights_executor','app_orders_privacy_executor')) EXECUTE FUNCTION app.orders_child_guard();

CREATE FUNCTION app.orders_erase_merchant(p_tenant uuid,p_request uuid,p_epoch bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE n bigint:=0; changed bigint;
BEGIN
 PERFORM app.tenant_lifecycle_read_fence(p_tenant);
 IF NOT EXISTS(SELECT FROM app.tenant_lifecycles WHERE tenant_id=p_tenant AND id=p_tenant
  AND request_id=p_request AND epoch=p_epoch AND state='executing' AND phase='executing') THEN
  RAISE EXCEPTION 'stale Orders erasure' USING ERRCODE='55000'; END IF;
 PERFORM id FROM app.orders WHERE tenant_id=p_tenant ORDER BY id FOR UPDATE;
 UPDATE app.order_adjustments SET reason='[erased]' WHERE tenant_id=p_tenant;
 GET DIAGNOSTICS changed=ROW_COUNT; n:=n+changed;
 UPDATE app.order_lines SET sku='[erased]',tax_snapshot='{"erased":true}'::jsonb WHERE tenant_id=p_tenant;
 GET DIAGNOSTICS changed=ROW_COUNT; n:=n+changed;
 UPDATE app.orders SET guest_contact=CASE WHEN guest_contact IS NULL THEN NULL
  ELSE jsonb_build_object('email','erased-'||id::text||'@invalid.test','erased',true) END,
  shipping_snapshot=jsonb_build_object('erased',true,'amount_minor',shipping_minor,'currency',currency) WHERE tenant_id=p_tenant;
 GET DIAGNOSTICS changed=ROW_COUNT; n:=n+changed;
 RETURN n;
END $$;
GRANT CREATE ON SCHEMA app TO app_orders_executor,app_orders_privacy_executor;
ALTER FUNCTION app.orders_merchant_mutation_fence(boolean) OWNER TO app_orders_executor;
ALTER FUNCTION app.orders_merchant_obligations(uuid) OWNER TO app_orders_executor;
ALTER FUNCTION app.orders_erase_merchant(uuid,uuid,bigint) OWNER TO app_orders_privacy_executor;
ALTER FUNCTION app.orders_merchant_scrub_guard() OWNER TO app_orders_privacy_executor;
REVOKE CREATE ON SCHEMA app FROM app_orders_executor,app_orders_privacy_executor;
REVOKE ALL ON FUNCTION app.orders_merchant_mutation_fence(boolean),app.orders_merchant_obligations(uuid),
 app.orders_erase_merchant(uuid,uuid,bigint),app.orders_merchant_scrub_guard() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.orders_merchant_obligations(uuid) TO app_runtime;
-- No caller receives orders_erase_merchant until MDEL-26's restricted worker.
