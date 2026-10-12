-- Pricing contract: exact tier uniqueness, money/time/target checks, tenant FKs and RLS.
-- Depends on the money_pricing migration; called by check_pg on an expendable PG18.
-- Fixtures roll back; a different failure must never masquerade as the expected guard.
BEGIN;
DO $pricing$
DECLARE
 t uuid:=uuidv7(); foreign_t uuid:=uuidv7(); p uuid; sku uuid; foreign_sku uuid;
 list_a uuid:='00000000-0000-0000-0000-000000000001';
 list_b uuid:='00000000-0000-0000-0000-000000000002';
 selected uuid; amount bigint; n integer;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'pricing-A'),(foreign_t,'pricing-B');
 INSERT INTO app.products(tenant_id,product_code) VALUES(t,'P') RETURNING id INTO p;
 INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(t,p,'SKU','default') RETURNING id INTO sku;
 INSERT INTO app.products(tenant_id,product_code) VALUES(foreign_t,'P') RETURNING id INTO p;
 INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES(foreign_t,p,'SKU','default') RETURNING id INTO foreign_sku;
 INSERT INTO app.price_lists(tenant_id,id,currency,status,starts_at,priority)
 VALUES(t,list_a,'USD','active','2026-01-01Z',5),(t,list_b,'USD','active','2026-01-01Z',5);
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
 VALUES(t,list_a,sku,1,150),(t,list_a,sku,10,120),(t,list_b,sku,1,100);
 BEGIN
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  VALUES(t,list_a,sku,10,110);
  RAISE EXCEPTION 'PRICING_DUPLICATE_TIER_ACCEPTED';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  VALUES(t,list_a,sku,20,-1);
  RAISE EXCEPTION 'PRICING_NEGATIVE_AMOUNT_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  VALUES(t,list_a,sku,0,1);
  RAISE EXCEPTION 'PRICING_ZERO_QUANTITY_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  VALUES(t,list_a,foreign_sku,1,1);
  RAISE EXCEPTION 'PRICING_FOREIGN_SKU_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
  VALUES(foreign_t,list_a,foreign_sku,1,1);
  RAISE EXCEPTION 'PRICING_FOREIGN_LIST_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at,ends_at)
  VALUES(t,'USD','2026-01-02Z','2026-01-01Z');
  RAISE EXCEPTION 'PRICING_INVALID_WINDOW_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(t,'usd','2026-01-01Z');
  RAISE EXCEPTION 'PRICING_INVALID_CURRENCY_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at,audience)
  VALUES(t,'USD','2026-01-01Z','buyer_company');
  RAISE EXCEPTION 'PRICING_MISSING_TARGET_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at,status) VALUES(t,'USD','2026-01-01Z','unknown');
  RAISE EXCEPTION 'PRICING_INVALID_STATUS_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 -- Deterministic tie and tier: priority first, smaller UUID, largest eligible tier.
 SELECT l.id,e.unit_amount INTO selected,amount FROM app.price_lists l JOIN app.price_entries e
 ON(e.tenant_id,e.price_list_id)=(l.tenant_id,l.id)
 WHERE l.tenant_id=t AND e.sku_id=sku AND e.min_quantity<=10
 ORDER BY l.priority DESC,l.id ASC,e.min_quantity DESC LIMIT 1;
 IF selected IS DISTINCT FROM list_a OR amount IS DISTINCT FROM 120 THEN RAISE EXCEPTION 'PRICING_TIE_OR_TIER'; END IF;
 UPDATE app.price_lists SET priority=6 WHERE tenant_id=t AND id=list_b;
 SELECT l.id INTO selected FROM app.price_lists l JOIN app.price_entries e
 ON(e.tenant_id,e.price_list_id)=(l.tenant_id,l.id)
 WHERE l.tenant_id=t AND e.sku_id=sku AND e.min_quantity<=10
 ORDER BY l.priority DESC,l.id ASC,e.min_quantity DESC LIMIT 1;
 IF selected IS DISTINCT FROM list_b THEN RAISE EXCEPTION 'PRICING_PRIORITY'; END IF;
 INSERT INTO app.price_lists(tenant_id,id,currency,starts_at) VALUES(foreign_t,list_a,'USD','2026-01-01Z');
 INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount)
 VALUES(foreign_t,list_a,foreign_sku,1,999);
 PERFORM set_config('app.tenant_id',t::text,true);
 SET LOCAL ROLE app_runtime;
 SELECT count(*) INTO n FROM app.price_lists;
 IF n<>2 THEN RAISE EXCEPTION 'PRICING_TENANT_READ: %',n; END IF;
 SELECT count(*) INTO n FROM app.price_entries;
 IF n<>3 THEN RAISE EXCEPTION 'PRICING_ENTRY_READ: %',n; END IF;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(t,'USD','2026-01-01Z');
  RAISE EXCEPTION 'PRICING_RUNTIME_WRITE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 SELECT count(*) INTO n FROM app.price_lists;
 IF n<>0 THEN RAISE EXCEPTION 'PRICING_UNSET_TENANT'; END IF;
 SELECT count(*) INTO n FROM app.price_entries;
 IF n<>0 THEN RAISE EXCEPTION 'PRICING_UNSET_ENTRY_TENANT'; END IF;
 RESET ROLE;
END $pricing$;
ROLLBACK;
SELECT 'PASS: pricing tiers, exact amounts, tenant foreign keys, read-only RLS' AS result;
