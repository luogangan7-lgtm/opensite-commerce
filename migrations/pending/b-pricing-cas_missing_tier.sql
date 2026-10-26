-- stable-id: b-pricing-cas
-- depends-on: admin-pricing-1
-- Responsibility: missing-tier clear is a validated no-op, preserving list CAS/version.
-- Dependencies: pricing tables and tenant settlement currency; caller: staff pricing.
-- Lock order: tenants FOR SHARE -> price_lists FOR UPDATE -> price_entries writes.
-- write_entry's preliminary list read is nonlocking existence/tenant filtering only;
-- every currency/version/status decision is re-read after taking the ordered locks.
-- PostgreSQL requires UPDATE on any column for FOR SHARE. The NOLOGIN executor's
-- settlement_currency grant is solely for this lock; no routine may write tenants
-- (exact function/column authority and reviewed currency body in db_role_matrix.sql).
-- Compatibility: same arguments/result; rollback: new forward migration, no data loss.
CREATE OR REPLACE FUNCTION app.pricing_write_entry(p_list uuid,p_sku uuid,p_min bigint,p_amount bigint,p_version bigint) RETURNS jsonb
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
 IF p_amount IS NULL THEN
  DELETE FROM app.price_entries WHERE tenant_id=t AND price_list_id=p_list AND sku_id=p_sku AND min_quantity=p_min;
  IF NOT FOUND THEN RETURN jsonb_strip_nulls(to_jsonb(r)-'tenant_id'); END IF;
 ELSE
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES(t,p_list,p_sku,p_min,p_amount)
  ON CONFLICT(tenant_id,price_list_id,sku_id,min_quantity) DO UPDATE SET unit_amount=excluded.unit_amount;
 END IF;
 UPDATE app.price_lists SET version=version+1 WHERE tenant_id=t AND id=p_list RETURNING * INTO r;
 RETURN jsonb_strip_nulls(to_jsonb(r)-'tenant_id');
END $$;
