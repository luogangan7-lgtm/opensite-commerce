-- Responsibility: additive disposable C-24 SKU, pricing, tax and shipping witnesses.
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
   AND method='fixed' AND currency='USD' AND amount=500 AND min_weight_g IS NULL AND max_weight_g IS NULL AND free_over_amount IS NULL) THEN
  RAISE EXCEPTION 'C24_CONFIG_DRIFT';
 END IF;
END $verify$;
-- C-19a owns separate tenants: this is webhook acceptance, not a purchase UI seed.
DO $payment_seed$
DECLARE n integer; suffix text; t uuid; sku uuid; oid uuid; cid uuid; r app.inventory_reservations; o app.orders; account_code text; account_kind text;
BEGIN
 FOR n IN 1..2 LOOP
  suffix:=lpad(n::text,12,'0');
  t:=('c1900010-0000-4000-8000-'||suffix)::uuid;
  sku:=('c1900002-0000-4000-8000-'||suffix)::uuid;
  oid:=('c1900003-0000-4000-8000-'||suffix)::uuid;
  cid:=('c1900004-0000-4000-8000-'||suffix)::uuid;
  INSERT INTO app.tenants(id,name,settlement_currency) VALUES(t,'C19 webhook '||n,'USD') ON CONFLICT DO NOTHING;
  IF NOT EXISTS(SELECT FROM app.tenants WHERE id=t AND name='C19 webhook '||n AND settlement_currency='USD') THEN RAISE EXCEPTION 'C19_E2E_TENANT_DRIFT'; END IF;
  PERFORM set_config('app.tenant_id',t::text,true);
  INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES(t,('c1900001-0000-4000-8000-'||suffix)::uuid,'C19-WEBHOOK','ready') ON CONFLICT DO NOTHING;
  INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status) VALUES(t,sku,('c1900001-0000-4000-8000-'||suffix)::uuid,'C19-WEBHOOK','default','ready') ON CONFLICT DO NOTHING;
  IF NOT EXISTS(SELECT FROM app.products p WHERE p.tenant_id=t AND p.id=('c1900001-0000-4000-8000-'||suffix)::uuid AND p.product_code='C19-WEBHOOK' AND p.lifecycle_status='ready')
   OR NOT EXISTS(SELECT FROM app.product_variants v WHERE v.tenant_id=t AND v.id=sku AND v.product_id=('c1900001-0000-4000-8000-'||suffix)::uuid AND v.sku='C19-WEBHOOK' AND v.option_key='default' AND v.lifecycle_status='ready') THEN RAISE EXCEPTION 'C19_E2E_SKU_DRIFT'; END IF;
  FOR account_code,account_kind IN SELECT * FROM (VALUES('cash','asset'),('accounts_receivable','asset'),('revenue','revenue'),('tax_payable','liability'),('shipping_revenue','revenue')) AS accounts(code,kind) LOOP
   INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES(t,account_code,'USD',account_kind) ON CONFLICT DO NOTHING;
   IF NOT EXISTS(SELECT FROM app.ledger_accounts a WHERE a.tenant_id=t AND a.code=account_code AND a.currency='USD' AND a.account_type=account_kind AND a.status='active') THEN RAISE EXCEPTION 'C19_E2E_LEDGER_DRIFT'; END IF;
  END LOOP;
  INSERT INTO app.payment_provider_accounts(tenant_id,provider,provider_account_id,environment,enabled)
   VALUES(t,'fake','c19-e2e-'||CASE n WHEN 1 THEN 'desktop' ELSE 'mobile' END,'test',true) ON CONFLICT DO NOTHING;
  IF NOT EXISTS(SELECT FROM app.payment_provider_accounts WHERE tenant_id=t AND provider='fake' AND provider_account_id='c19-e2e-'||CASE n WHEN 1 THEN 'desktop' ELSE 'mobile' END AND environment='test' AND enabled) THEN RAISE EXCEPTION 'C19_E2E_ACCOUNT_DRIFT'; END IF;
  IF NOT EXISTS(SELECT FROM app.checkout_sessions WHERE tenant_id=t AND id=cid) THEN
   PERFORM app.inventory_adjust(sku,10,'C19 webhook seed',('c1900006-0000-4000-8000-'||suffix)::uuid,'inbound');
   r:=app.inventory_reserve(sku,2,'order',oid,'c19-e2e-hold',3600);
   o:=app.orders_create(oid,'c19-e2e-order',sha256('c19-e2e-order'::bytea),NULL,'{"email":"c19-webhook@example.invalid"}','USD',50,'{}',
    jsonb_build_array(jsonb_build_object('id',('c1900007-0000-4000-8000-'||suffix)::uuid,'sku_id',sku,'sku','C19-WEBHOOK','quantity',2,'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',20,'line_total_minor',220,'reservation_id',r.id,'tax_snapshot','{}'::jsonb)),'[]');
   INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version)
    VALUES(t,cid,'guest',repeat('a',64),'c19-e2e-checkout',sha256('c19-e2e-checkout'::bytea),('c1900005-0000-4000-8000-'||suffix)::uuid,1,oid,'{}','USD',270,200,20,50,1);
   UPDATE app.checkout_sessions SET phase='ready',provider_ref='fake_c19_e2e_'||n,start_result='{"state":"ready"}' WHERE tenant_id=t AND id=cid;
  END IF;
  IF NOT EXISTS(SELECT FROM app.checkout_sessions c JOIN app.orders o ON (o.tenant_id,o.id)=(c.tenant_id,c.order_id) WHERE c.tenant_id=t AND c.id=cid AND c.order_id=oid AND c.phase='ready' AND c.provider_ref='fake_c19_e2e_'||n AND c.amount_minor=270 AND c.currency='USD' AND c.payable_version=1 AND o.state='pending_payment' AND o.version=1 AND o.total_minor=270 AND o.sealed)
   OR NOT EXISTS(SELECT FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku AND on_hand=10 AND reserved=2) THEN RAISE EXCEPTION 'C19_E2E_CHECKOUT_DRIFT'; END IF;
 END LOOP;
END $payment_seed$;
COMMIT;
