-- stable-id: admin-pricing-1
-- depends-on: c15b-serving
-- Responsibility: staff price CAS capability; owns price_lists/price_entries writes.
-- Dependencies: tenant settlement identity and existing serving invalidation triggers.
-- Caller: pricing via authorized sqlc; runtime never receives raw table write grants.
ALTER TABLE app.price_lists ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_pricing_executor') THEN CREATE ROLE app_pricing_executor; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_pricing_executor'::regrole OR roleid='app_pricing_executor'::regrole) THEN
  RAISE EXCEPTION 'PRICING_ROLE_MEMBERSHIP_UNSAFE'; END IF;
END $$;
ALTER ROLE app_pricing_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_pricing_executor;
GRANT SELECT,INSERT,UPDATE ON app.price_lists TO app_pricing_executor;
GRANT SELECT,INSERT,UPDATE,DELETE ON app.price_entries TO app_pricing_executor;
GRANT SELECT(tenant_id,id) ON app.product_variants TO app_pricing_executor;
GRANT SELECT(id,settlement_currency),UPDATE(settlement_currency) ON app.tenants TO app_pricing_executor;
CREATE POLICY pricing_executor ON app.price_lists TO app_pricing_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY pricing_executor ON app.price_entries TO app_pricing_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY pricing_variant_reader ON app.product_variants FOR SELECT TO app_pricing_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);

CREATE FUNCTION app.pricing_currency() RETURNS text LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE currency text;
BEGIN
 SELECT settlement_currency INTO currency FROM app.tenants
 WHERE id=nullif(current_setting('app.tenant_id',true),'')::uuid FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRICING_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF currency IS NULL THEN RAISE EXCEPTION 'SETTLEMENT_CURRENCY_REQUIRED' USING ERRCODE='P4090'; END IF;
 RETURN currency;
END $$;
CREATE FUNCTION app.pricing_write_list(p_id uuid,p_version bigint,p_body jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; c text; r app.price_lists;
BEGIN
 c:=app.pricing_currency();
 IF p_id IS NOT NULL THEN
  SELECT * INTO r FROM app.price_lists WHERE tenant_id=t AND id=p_id FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'PRICING_NOT_FOUND' USING ERRCODE='P0002'; END IF;
  IF p_version IS DISTINCT FROM r.version THEN RAISE EXCEPTION 'PRICING_VERSION_CONFLICT' USING ERRCODE='P4120'; END IF;
  IF r.status='archived' THEN RAISE EXCEPTION 'PRICING_ARCHIVED' USING ERRCODE='P4092'; END IF;
  IF r.currency::text<>c THEN RAISE EXCEPTION 'PRICING_CURRENCY_MISMATCH' USING ERRCODE='P4091'; END IF;
 END IF;
 -- Fault probe: omit requested-currency equality admission; expected PG gate RED.
 IF NOT isfinite((p_body->>'starts_at')::timestamptz) OR NOT isfinite(coalesce((p_body->>'ends_at')::timestamptz,(p_body->>'starts_at')::timestamptz)) THEN
  RAISE EXCEPTION 'PRICING_WINDOW' USING ERRCODE='23514'; END IF;
 IF p_id IS NULL THEN
  INSERT INTO app.price_lists(tenant_id,currency,status,starts_at,ends_at,priority,audience,customer_group_id,buyer_company_id)
  VALUES(t,c,p_body->>'status',(p_body->>'starts_at')::timestamptz,(p_body->>'ends_at')::timestamptz,
   (p_body->>'priority')::integer,p_body->>'audience',(p_body->>'customer_group_id')::uuid,(p_body->>'buyer_company_id')::uuid) RETURNING * INTO r;
 ELSE
  UPDATE app.price_lists SET currency=c,status=p_body->>'status',starts_at=(p_body->>'starts_at')::timestamptz,
   ends_at=(p_body->>'ends_at')::timestamptz,priority=(p_body->>'priority')::integer,audience=p_body->>'audience',
   customer_group_id=(p_body->>'customer_group_id')::uuid,buyer_company_id=(p_body->>'buyer_company_id')::uuid,version=version+1
  WHERE tenant_id=t AND id=p_id RETURNING * INTO r;
 END IF;
 RETURN jsonb_strip_nulls(to_jsonb(r)-'tenant_id');
END $$;
CREATE FUNCTION app.pricing_write_entry(p_list uuid,p_sku uuid,p_min bigint,p_amount bigint,p_version bigint) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE t uuid:=nullif(current_setting('app.tenant_id',true),'')::uuid; c text; r app.price_lists;
BEGIN
 -- Resolve list before missing currency so foreign identifiers remain 404.
 SELECT * INTO r FROM app.price_lists WHERE tenant_id=t AND id=p_list;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRICING_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 c:=app.pricing_currency();
 SELECT * INTO r FROM app.price_lists WHERE tenant_id=t AND id=p_list FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'PRICING_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_version IS DISTINCT FROM r.version THEN RAISE EXCEPTION 'PRICING_VERSION_CONFLICT' USING ERRCODE='P4120'; END IF;
 IF r.status='archived' THEN RAISE EXCEPTION 'PRICING_ARCHIVED' USING ERRCODE='P4092'; END IF;
 IF r.currency::text<>c THEN RAISE EXCEPTION 'PRICING_CURRENCY_MISMATCH' USING ERRCODE='P4091'; END IF;
 IF NOT EXISTS(SELECT FROM app.product_variants WHERE tenant_id=t AND id=p_sku) THEN
  RAISE EXCEPTION 'PRICING_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_min IS NULL OR p_min<=0 OR p_amount<0 THEN RAISE EXCEPTION 'PRICING_AMOUNT' USING ERRCODE='23514'; END IF;
 IF p_amount IS NULL THEN DELETE FROM app.price_entries WHERE tenant_id=t AND price_list_id=p_list AND sku_id=p_sku AND min_quantity=p_min;
 ELSE
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES(t,p_list,p_sku,p_min,p_amount)
  ON CONFLICT(tenant_id,price_list_id,sku_id,min_quantity) DO UPDATE SET unit_amount=excluded.unit_amount;
 END IF;
 UPDATE app.price_lists SET version=version+1 WHERE tenant_id=t AND id=p_list RETURNING * INTO r;
 RETURN jsonb_strip_nulls(to_jsonb(r)-'tenant_id');
END $$;
ALTER FUNCTION app.pricing_currency() OWNER TO app_pricing_executor;
ALTER FUNCTION app.pricing_write_list(uuid,bigint,jsonb) OWNER TO app_pricing_executor;
ALTER FUNCTION app.pricing_write_entry(uuid,uuid,bigint,bigint,bigint) OWNER TO app_pricing_executor;
REVOKE ALL ON FUNCTION app.pricing_currency(),app.pricing_write_list(uuid,bigint,jsonb),app.pricing_write_entry(uuid,uuid,bigint,bigint,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.pricing_write_list(uuid,bigint,jsonb),app.pricing_write_entry(uuid,uuid,bigint,bigint,bigint) TO app_runtime;
