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
BEGIN
 IF app.cms_seal_skus(t,s,r,skus)<>1 THEN RAISE EXCEPTION 'SERVING_SEAL_COUNT'; END IF;
 -- Write-once: a second seal (even empty) and any UPDATE/DELETE are rejected.
 BEGIN PERFORM app.cms_seal_skus(t,s,r,'[]'); RAISE EXCEPTION 'SERVING_SEAL_TWICE'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN UPDATE app.site_release_skus SET sku_code='X'; RAISE EXCEPTION 'SERVING_SKUS_MUTABLE'; EXCEPTION WHEN insufficient_privilege OR object_not_in_prerequisite_state THEN NULL; END;
 BEGIN INSERT INTO app.storefront_serving(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,built_at)
  VALUES(t,s,r,1,1,'{}','{}',sha256('{}'),now()); RAISE EXCEPTION 'SERVING_DIRECT_WRITE'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 SELECT publication_revision INTO pub FROM app.sites WHERE tenant_id=t AND id=s;
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub,0,'\x7b7d','\x7b7d',now()+interval '16 minutes'); RAISE EXCEPTION 'SERVING_LIFETIME_UNBOUNDED'; EXCEPTION WHEN invalid_parameter_value THEN NULL; END;
 gen:=app.cms_serving_put(t,s,r,pub,0,convert_to('{"a":1}'||chr(10),'UTF8'),convert_to('{"a":0}'||chr(10),'UTF8'),now()+interval '10 minutes');
 IF gen<>1 THEN RAISE EXCEPTION 'SERVING_FIRST_GENERATION'; END IF;
 -- G5: stale generation or stale publication can never overwrite the newer row.
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub,0,'\x7b7d','\x7b7d',NULL); RAISE EXCEPTION 'SERVING_STALE_GENERATION'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN PERFORM app.cms_serving_put(t,s,r,pub-1,1,'\x7b7d','\x7b7d',NULL); RAISE EXCEPTION 'SERVING_STALE_PUBLICATION'; EXCEPTION WHEN serialization_failure THEN NULL; END;
 BEGIN PERFORM app.cms_serving_put(current_setting('test.other')::uuid,s,r,pub,1,'\x7b7d','\x7b7d',NULL); RAISE EXCEPTION 'SERVING_CROSS_TENANT'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 IF (SELECT convert_from(body,'UTF8') FROM app.storefront_serving WHERE tenant_id=t AND site_id=s)<>'{"a":1}'||chr(10)
  OR (SELECT serving_generation FROM app.storefront_serving WHERE tenant_id=t AND site_id=s)<>1
  OR (SELECT fallback_sha256<>sha256(fallback_body) OR body_sha256<>sha256(body) OR dirty FROM app.storefront_serving WHERE tenant_id=t AND site_id=s) THEN RAISE EXCEPTION 'SERVING_OVERWRITTEN'; END IF;
END $runtime$;
RESET ROLE;
-- G11: a mutation of a member SKU invalidates the row in the same transaction (dirty,
-- lifetime bounded by the 30 s grace); an unrelated tenant's row is untouched.
UPDATE app.product_variants SET lifecycle_status='archived' WHERE tenant_id=:'tenant' AND id=:'sku';
DO $invalidate$ BEGIN
 IF NOT (SELECT dirty AND valid_until<=now()+interval '30 seconds' FROM app.storefront_serving WHERE tenant_id=current_setting('test.tenant')::uuid) THEN
  RAISE EXCEPTION 'SERVING_NOT_INVALIDATED'; END IF;
END $invalidate$;
SELECT set_config('app.tenant_id','',true),set_config('app.public_hostname','serving-a.example',true);
SET LOCAL ROLE app_domain_resolver;
DO $public$
BEGIN
 IF (SELECT count(*) FROM app.storefront_serving)<>1 THEN RAISE EXCEPTION 'SERVING_PUBLIC_ROW'; END IF;
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
 IF (SELECT count(*) FROM app.storefront_serving)<>0 THEN RAISE EXCEPTION 'SERVING_PUBLIC_HOST_SCOPE'; END IF;
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
ROLLBACK;
SELECT 'PASS: storefront serving write-once SKUs, CAS, anonymous scope' AS result;
