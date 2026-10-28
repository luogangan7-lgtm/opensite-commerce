-- Responsibility: storefront serving SQL gates (ADR_STOREFRONT_SERVING G4/G5, sealed SKUs).
-- Dependencies: all forward migrations through storefront_serving; caller: check_pg.sh.
-- Invariants: SKU membership write-once in the creating transaction; serving CAS rejects
-- stale publication/generation; the anonymous resolver reads only the current serving row.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES ('serving-a') RETURNING id AS tenant \gset
INSERT INTO app.tenants(name) VALUES ('serving-b') RETURNING id AS other_tenant \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES (:'tenant','A') RETURNING id AS org \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES (:'tenant',:'org','A') RETURNING id AS site \gset
INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES (:'tenant',:'site','serving-a.example','verified',now());
INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES (:'tenant','P','ready') RETURNING id AS product \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status) VALUES (:'tenant',:'product','SKU-1','a','ready') RETURNING id AS sku \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES (:'tenant',:'site',1,'t','{"version":1,"locale":"en","title":"A","description":"","navigation":[],"products":[]}',decode(repeat('11',32),'hex')) RETURNING id AS release \gset
SELECT set_config('app.tenant_id',:'tenant',true);
SELECT app.publish_release(:'tenant',:'site',:'release',NULL);
SELECT set_config('test.tenant',:'tenant',true),set_config('test.other',:'other_tenant',true),set_config('test.site',:'site',true),
 set_config('test.release',:'release',true),set_config('test.product',:'product',true),set_config('test.sku',:'sku',true);
SET LOCAL ROLE app_runtime;
DO $runtime$
DECLARE t uuid:=current_setting('test.tenant')::uuid; s uuid:=current_setting('test.site')::uuid; r uuid:=current_setting('test.release')::uuid;
 skus jsonb:=jsonb_build_array(jsonb_build_object('product_id',current_setting('test.product'),'sku_id',current_setting('test.sku'),'sku_code','SKU-1','option_values','[]'::jsonb));
 pub bigint; gen bigint;
 body bytea:=convert_to(jsonb_build_object('tenant_id',current_setting('test.tenant'),'site_id',current_setting('test.site'),'release_id',current_setting('test.release'),'v',1)::text||chr(10),'UTF8');
 fallback bytea:=convert_to(jsonb_build_object('tenant_id',current_setting('test.tenant'),'site_id',current_setting('test.site'),'release_id',current_setting('test.release'),'v',0)::text||chr(10),'UTF8');
 forged bytea:=convert_to(jsonb_build_object('tenant_id',current_setting('test.other'),'site_id',current_setting('test.site'),'release_id',current_setting('test.release'))::text,'UTF8');
BEGIN
 -- Members must be real variants with their real code and product.
 BEGIN PERFORM app.cms_seal_skus(t,s,r,jsonb_build_array(jsonb_build_object('product_id',current_setting('test.product'),'sku_id',current_setting('test.sku'),'sku_code','FORGED','option_values','[]'::jsonb)));
  RAISE EXCEPTION 'SERVING_SEAL_FORGED_CODE'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 IF app.cms_seal_skus(t,s,r,skus)<>1 THEN RAISE EXCEPTION 'SERVING_SEAL_COUNT'; END IF;
 -- Write-once: a second seal (even empty) and any UPDATE/DELETE are rejected.
 BEGIN PERFORM app.cms_seal_skus(t,s,r,'[]'); RAISE EXCEPTION 'SERVING_SEAL_TWICE'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.site_release_skus SET sku_code='X'; RAISE EXCEPTION 'SERVING_SKUS_MUTABLE'; EXCEPTION WHEN insufficient_privilege OR object_not_in_prerequisite_state THEN NULL; END;
 BEGIN INSERT INTO app.storefront_serving(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,built_at)
  VALUES(t,s,r,1,1,'{}','{}',sha256('{}'),now()); RAISE EXCEPTION 'SERVING_DIRECT_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN INSERT INTO app.serving_invalidations(tenant_id,site_id,reason) VALUES(t,s,'product'); RAISE EXCEPTION 'SERVING_DIRECT_LOG'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN DELETE FROM app.serving_invalidations; RAISE EXCEPTION 'SERVING_DIRECT_PRUNE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 SELECT publication_revision INTO pub FROM app.sites WHERE tenant_id=t AND id=s;
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub,0,body,fallback,now()+interval '16 minutes','{}'); RAISE EXCEPTION 'SERVING_LIFETIME_UNBOUNDED'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 -- P2-5: a body naming another tenant is rejected.
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub,0,forged,fallback,now()+interval '10 minutes','{}'); RAISE EXCEPTION 'SERVING_FORGED_BODY'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 gen:=app.cms_serving_put(t,s,r,pub,0,body,fallback,now()+interval '10 minutes','{}');
 IF gen<>1 THEN RAISE EXCEPTION 'SERVING_FIRST_GENERATION'; END IF;
 -- G5: stale generation or stale publication can never overwrite the newer row.
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub,0,body,fallback,now()+interval '1 minute','{}'); RAISE EXCEPTION 'SERVING_STALE_GENERATION'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub-1,1,body,fallback,now()+interval '1 minute','{}'); RAISE EXCEPTION 'SERVING_STALE_PUBLICATION'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN PERFORM app.cms_serving_put(current_setting('test.other')::uuid,s,r,pub,1,body,fallback,now()+interval '1 minute','{}'); RAISE EXCEPTION 'SERVING_CROSS_TENANT'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 IF (SELECT sv.body FROM app.storefront_serving sv WHERE sv.tenant_id=t AND sv.site_id=s)<>body
  OR (SELECT serving_generation FROM app.storefront_serving WHERE tenant_id=t AND site_id=s)<>1
  OR (SELECT sv.fallback_sha256<>sha256(sv.fallback_body) OR sv.body_sha256<>sha256(sv.body) FROM app.storefront_serving sv WHERE sv.tenant_id=t AND sv.site_id=s) THEN RAISE EXCEPTION 'SERVING_OVERWRITTEN'; END IF;
END $runtime$;
RESET ROLE;
-- G11: a mutation of a member SKU appends one invalidation in the same transaction and
-- leaves the serving row untouched (no shared-row lock on the writer).
UPDATE app.product_variants SET lifecycle_status='archived' WHERE tenant_id=:'tenant' AND id=:'sku';
DO $invalidate$ BEGIN
 IF (SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=current_setting('test.tenant')::uuid AND reason='variant')<>1
  OR (SELECT serving_generation FROM app.storefront_serving WHERE tenant_id=current_setting('test.tenant')::uuid)<>1 THEN
  RAISE EXCEPTION 'SERVING_NOT_INVALIDATED'; END IF;
END $invalidate$;
-- Age it past the 30 s grace: the public role sees that an old invalidation exists (its
-- read then serves the fallback), but not its reason or sequence.
UPDATE app.serving_invalidations SET created_at=now()-interval '31 seconds' WHERE tenant_id=:'tenant';
SELECT set_config('app.tenant_id','',true),set_config('app.public_hostname','serving-a.example',true);
SET LOCAL ROLE app_domain_resolver;
DO $public$
BEGIN
 IF (SELECT count(*) FROM app.storefront_serving)<>1 THEN RAISE EXCEPTION 'SERVING_PUBLIC_ROW'; END IF;
 IF (SELECT count(*) FROM app.serving_invalidations WHERE created_at<now()-interval '30 seconds')<>1 THEN RAISE EXCEPTION 'SERVING_PUBLIC_INVALIDATION'; END IF;
 BEGIN PERFORM reason FROM app.serving_invalidations; RAISE EXCEPTION 'SERVING_PUBLIC_REASON'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 -- G4: no live catalog, pricing, inventory or membership data for the anonymous role.
 BEGIN PERFORM id FROM app.product_variants; RAISE EXCEPTION 'SERVING_PUBLIC_VARIANTS'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM id FROM app.products; RAISE EXCEPTION 'SERVING_PUBLIC_PRODUCTS'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM id FROM app.price_lists; RAISE EXCEPTION 'SERVING_PUBLIC_PRICE_LISTS'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM unit_amount FROM app.price_entries; RAISE EXCEPTION 'SERVING_PUBLIC_PRICES'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM on_hand FROM app.stock_levels; RAISE EXCEPTION 'SERVING_PUBLIC_STOCK'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM quantity FROM app.inventory_reservations; RAISE EXCEPTION 'SERVING_PUBLIC_RESERVATIONS'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM sku_code FROM app.site_release_skus; RAISE EXCEPTION 'SERVING_PUBLIC_MEMBERSHIP'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN PERFORM serving_generation FROM app.storefront_serving; RAISE EXCEPTION 'SERVING_PUBLIC_GENERATION'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.public_hostname','unknown.example',true);
 IF (SELECT count(*) FROM app.storefront_serving)<>0 OR (SELECT count(*) FROM app.serving_invalidations)<>0 THEN RAISE EXCEPTION 'SERVING_PUBLIC_HOST_SCOPE'; END IF;
END $public$;
RESET ROLE;
-- A newer publication makes the old serving row invisible to the public role.
SELECT set_config('app.tenant_id',:'tenant',true);
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES (:'tenant',:'site',2,'t','{"version":1,"locale":"en","title":"A2","description":"","navigation":[],"products":[]}',decode(repeat('22',32),'hex')) RETURNING id AS release2 \gset
SELECT app.publish_release(:'tenant',:'site',:'release2',:'release');
SELECT set_config('app.tenant_id','',true),set_config('app.public_hostname','serving-a.example',true);
SET LOCAL ROLE app_domain_resolver;
DO $stale$ BEGIN
 IF (SELECT count(*) FROM app.storefront_serving)<>0 THEN RAISE EXCEPTION 'SERVING_PUBLIC_STALE_ROW'; END IF;
END $stale$;
RESET ROLE;
-- Staff writes reuse the existing invalidation triggers, without replacing serving.
SELECT set_config('app.tenant_id',:'tenant',true),set_config('pricing.sku',:'sku',true);
UPDATE app.tenants SET settlement_currency='USD' WHERE id=:'tenant';
SET LOCAL ROLE app_runtime;
DO $prices$ DECLARE l uuid; BEGIN
 l:=(app.pricing_write_list(NULL,0,'{"currency":"USD","status":"active","starts_at":"2026-01-01T00:00:00Z","priority":0,"audience":"all"}')->>'id')::uuid;
 PERFORM app.pricing_write_entry(l,current_setting('pricing.sku')::uuid,1,100,1);
 PERFORM app.pricing_write_entry(l,current_setting('pricing.sku')::uuid,1,NULL,2);
END $prices$;
RESET ROLE;
DO $price_invalidation$ BEGIN
 IF (SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=current_setting('test.tenant')::uuid AND reason='price_list')<>3
  OR (SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=current_setting('test.tenant')::uuid AND reason='price_entry')<>2
  OR (SELECT serving_generation FROM app.storefront_serving WHERE tenant_id=current_setting('test.tenant')::uuid)<>1 THEN
  RAISE EXCEPTION 'STAFF_PRICING_GATE serving invalidation'; END IF;
END $price_invalidation$;
-- G15: changing a price-entry SKU invalidates both sides, once per site.
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key,lifecycle_status)
 VALUES(:'tenant',:'product','SKU-2','b','ready') RETURNING id AS sku2 \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'tenant',:'org','SKU2') RETURNING id AS site2 \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(:'tenant',:'site2',1,'t','{}',sha256('{}'::bytea)) RETURNING id AS rel2 \gset
INSERT INTO app.site_release_skus(tenant_id,site_id,release_id,product_id,sku_id,ordinal,sku_code,option_values)
 VALUES(:'tenant',:'site2',:'rel2',:'product',:'sku2',0,'SKU-2','[]');
INSERT INTO app.storefront_serving(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,built_at)
 VALUES(:'tenant',:'site2',:'rel2',1,1,'{}','{}',sha256('{}'::bytea),sha256('{}'::bytea),now()+interval '1 minute',now());
INSERT INTO app.price_lists(tenant_id,currency,starts_at) VALUES(:'tenant','USD',now()) RETURNING id AS list2 \gset
INSERT INTO app.price_entries(tenant_id,price_list_id,sku_id,min_quantity,unit_amount) VALUES(:'tenant',:'list2',:'sku',1,111);
DELETE FROM app.serving_invalidations WHERE tenant_id=:'tenant';
UPDATE app.price_entries SET sku_id=:'sku2' WHERE tenant_id=:'tenant' AND price_list_id=:'list2';
DO $both$ BEGIN
 IF (SELECT count(*) FROM app.serving_invalidations WHERE tenant_id=current_setting('test.tenant')::uuid AND reason='price_entry')<>2
  OR (SELECT count(DISTINCT site_id) FROM app.serving_invalidations WHERE tenant_id=current_setting('test.tenant')::uuid AND reason='price_entry')<>2 THEN
  RAISE EXCEPTION 'PRICE_ENTRY_OLD_NEW_INVALIDATION'; END IF;
END $both$;
ROLLBACK;
SELECT 'PASS: storefront serving write-once SKUs, CAS, anonymous scope' AS result;
