-- Responsibility: additive disposable C-24 SKU, pricing, tax, shipping, stock and ledger witnesses.
-- Dependencies: all migrations and tests/fixtures/storefront_seed.sql; caller: CI run.py.
-- Invariant: fixed fixture ownership and values are checked; reruns never repair drift.
\set ON_ERROR_STOP on
BEGIN;
SELECT pg_advisory_xact_lock(18002401::bigint);
DO $owned$
BEGIN
 IF NOT EXISTS(SELECT FROM app.tenants WHERE id='c0900001-0000-4000-8000-000000000001' AND name='C09 storefront fixture 1') THEN
  RAISE EXCEPTION 'C24_SAMPLE_TENANT_NOT_OWNED';
 END IF;
END $owned$;
INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400008-0000-4000-8000-000000000001','c0900007-0000-4000-8000-000000000001','C24-CANVAS-TOTE','default','ready'),
 ('c0900001-0000-4000-8000-000000000001','c2400008-0000-4000-8000-000000000002','c0900007-0000-4000-8000-000000000002','C24-CERAMIC-MUG','default','ready') ON CONFLICT DO NOTHING;
INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400009-0000-4000-8000-000000000001','USD','active','2020-01-01T00:00:00Z') ON CONFLICT DO NOTHING;
INSERT INTO app.price_entries(tenant_id,id,price_list_id,sku_id,min_quantity,unit_amount) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400010-0000-4000-8000-000000000001','c2400009-0000-4000-8000-000000000001','c2400008-0000-4000-8000-000000000001',1,2400),
 ('c0900001-0000-4000-8000-000000000001','c2400010-0000-4000-8000-000000000002','c2400009-0000-4000-8000-000000000001','c2400008-0000-4000-8000-000000000002',1,1800) ON CONFLICT DO NOTHING;
INSERT INTO app.tax_zones(tenant_id,id,name) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400011-0000-4000-8000-000000000001','C24 sample US') ON CONFLICT DO NOTHING;
INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code,subdivision) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400011-0000-4000-8000-000000000001','US','') ON CONFLICT DO NOTHING;
INSERT INTO app.tax_categories(tenant_id,code,name,is_exempt) VALUES
 ('c0900001-0000-4000-8000-000000000001','standard','C24 sample standard',false) ON CONFLICT DO NOTHING;
INSERT INTO app.tax_rates(tenant_id,id,zone_id,category_code,rate_bp,valid_from) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400012-0000-4000-8000-000000000001','c2400011-0000-4000-8000-000000000001','standard',1000,'2020-01-01T00:00:00Z') ON CONFLICT DO NOTHING;
INSERT INTO app.tax_settings(tenant_id,prices_include_tax) VALUES
 ('c0900001-0000-4000-8000-000000000001',false) ON CONFLICT DO NOTHING;
INSERT INTO app.shipping_zones(tenant_id,id,name) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400013-0000-4000-8000-000000000001','C24 sample US') ON CONFLICT DO NOTHING;
INSERT INTO app.shipping_zone_destinations(tenant_id,zone_id,country_code,subdivision_code) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400013-0000-4000-8000-000000000001','US','') ON CONFLICT DO NOTHING;
INSERT INTO app.shipping_profiles(tenant_id,id,name,currency) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400014-0000-4000-8000-000000000001','C24 sample shipping','USD') ON CONFLICT DO NOTHING;
INSERT INTO app.shipping_rates(tenant_id,id,profile_id,zone_id,method,currency,amount) VALUES
 ('c0900001-0000-4000-8000-000000000001','c2400015-0000-4000-8000-000000000001','c2400014-0000-4000-8000-000000000001','c2400013-0000-4000-8000-000000000001','fixed','USD',500) ON CONFLICT DO NOTHING;
UPDATE app.tenants SET settlement_currency='USD'
 WHERE id='c0900001-0000-4000-8000-000000000001' AND settlement_currency IS NULL;
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES
 ('c0900001-0000-4000-8000-000000000001','cash','USD','asset'),
 ('c0900001-0000-4000-8000-000000000001','accounts_receivable','USD','asset'),
 ('c0900001-0000-4000-8000-000000000001','revenue','USD','revenue'),
 ('c0900001-0000-4000-8000-000000000001','tax_payable','USD','liability'),
 ('c0900001-0000-4000-8000-000000000001','shipping_revenue','USD','revenue') ON CONFLICT DO NOTHING;
SELECT set_config('app.tenant_id','c0900001-0000-4000-8000-000000000001',true);
SET LOCAL ROLE app_inventory_manager;
DO $stock$
BEGIN
 IF NOT EXISTS(SELECT FROM app.stock_levels WHERE tenant_id='c0900001-0000-4000-8000-000000000001'
     AND sku_id='c2400008-0000-4000-8000-000000000001') THEN
  PERFORM app.inventory_adjust('c2400008-0000-4000-8000-000000000001',20,
   'C24 E2E initial tote stock','c2400016-0000-4000-8000-000000000001','inbound');
 END IF;
END $stock$;
RESET ROLE;
DO $verify$
DECLARE t uuid := 'c0900001-0000-4000-8000-000000000001'; n integer;
BEGIN
 FOR n IN 1..2 LOOP
  IF NOT EXISTS(SELECT FROM app.product_variants WHERE tenant_id=t AND id=('c2400008-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid
   AND product_id=('c0900007-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid
   AND sku=CASE n WHEN 1 THEN 'C24-CANVAS-TOTE' ELSE 'C24-CERAMIC-MUG' END AND option_key='default' AND lifecycle_status='ready')
   OR NOT EXISTS(SELECT FROM app.price_entries WHERE tenant_id=t AND id=('c2400010-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid
    AND price_list_id='c2400009-0000-4000-8000-000000000001' AND sku_id=('c2400008-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid
    AND min_quantity=1 AND unit_amount=CASE n WHEN 1 THEN 2400 ELSE 1800 END) THEN
   RAISE EXCEPTION 'C24_SKU_PRICE_DRIFT';
  END IF;
 END LOOP;
 IF NOT EXISTS(SELECT FROM app.price_lists WHERE tenant_id=t AND id='c2400009-0000-4000-8000-000000000001'
  AND currency='USD' AND status='active' AND starts_at='2020-01-01T00:00:00Z' AND ends_at IS NULL AND priority=0 AND audience='all')
  OR NOT EXISTS(SELECT FROM app.tax_zones WHERE tenant_id=t AND id='c2400011-0000-4000-8000-000000000001' AND name='C24 sample US')
  OR NOT EXISTS(SELECT FROM app.tax_zone_locations WHERE tenant_id=t AND zone_id='c2400011-0000-4000-8000-000000000001' AND country_code='US' AND subdivision='')
  OR NOT EXISTS(SELECT FROM app.tax_categories WHERE tenant_id=t AND code='standard' AND name='C24 sample standard' AND NOT is_exempt)
  OR NOT EXISTS(SELECT FROM app.tax_rates WHERE tenant_id=t AND id='c2400012-0000-4000-8000-000000000001'
   AND zone_id='c2400011-0000-4000-8000-000000000001' AND category_code='standard' AND rate_bp=1000
   AND valid_from='2020-01-01T00:00:00Z' AND valid_until IS NULL)
  OR NOT EXISTS(SELECT FROM app.tax_settings WHERE tenant_id=t AND NOT prices_include_tax)
  OR NOT EXISTS(SELECT FROM app.shipping_zones WHERE tenant_id=t AND id='c2400013-0000-4000-8000-000000000001' AND name='C24 sample US')
  OR NOT EXISTS(SELECT FROM app.shipping_zone_destinations WHERE tenant_id=t AND zone_id='c2400013-0000-4000-8000-000000000001' AND country_code='US' AND subdivision_code='')
  OR NOT EXISTS(SELECT FROM app.shipping_profiles WHERE tenant_id=t AND id='c2400014-0000-4000-8000-000000000001' AND name='C24 sample shipping' AND currency='USD')
  OR NOT EXISTS(SELECT FROM app.shipping_rates WHERE tenant_id=t AND id='c2400015-0000-4000-8000-000000000001'
   AND profile_id='c2400014-0000-4000-8000-000000000001' AND zone_id='c2400013-0000-4000-8000-000000000001'
   AND method='fixed' AND currency='USD' AND amount=500 AND min_weight_g IS NULL AND max_weight_g IS NULL AND free_over_amount IS NULL)
  OR NOT EXISTS(SELECT FROM app.tenants WHERE id=t AND settlement_currency='USD')
  OR (SELECT count(*) FROM app.ledger_accounts WHERE tenant_id=t AND status='active' AND currency='USD'
   AND (code,account_type) IN (('cash','asset'),('accounts_receivable','asset'),('revenue','revenue'),
    ('tax_payable','liability'),('shipping_revenue','revenue')))<>5
  OR NOT EXISTS(SELECT FROM app.stock_levels WHERE tenant_id=t
   AND sku_id='c2400008-0000-4000-8000-000000000001' AND on_hand=20 AND reserved=0)
  OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t
   AND sku_id='c2400008-0000-4000-8000-000000000001' AND kind='inbound' AND quantity=20
   AND owner_id='c2400016-0000-4000-8000-000000000001')<>1 THEN
  RAISE EXCEPTION 'C24_CONFIG_DRIFT';
 END IF;
END $verify$;
COMMIT;
