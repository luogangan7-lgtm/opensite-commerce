-- Responsibility: sealed SKU membership per release and the rebuildable public serving row.
-- Dependencies: 006/010/024/031; caller: publish WriteStore (tenant tx) and public read.
-- Invariants: SKU membership is write-once in the transaction that created the release;
-- serving writes CAS on (publication_revision, serving_generation); the anonymous
-- resolver reads only domain map, publication and the current serving row (no live
-- catalog/pricing/inventory). See docs/architecture/ADR_STOREFRONT_SERVING.md.
CREATE TABLE app.site_release_skus (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 release_id uuid NOT NULL,
 product_id uuid NOT NULL,
 sku_id uuid NOT NULL,
 ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 4999),
 sku_code text NOT NULL CHECK (btrim(sku_code)<>'' AND length(sku_code)<=255),
 option_values jsonb NOT NULL CHECK (jsonb_typeof(option_values)='array' AND jsonb_array_length(option_values)<=20),
 PRIMARY KEY (tenant_id,site_id,release_id,sku_id),
 UNIQUE (tenant_id,site_id,release_id,ordinal),
 FOREIGN KEY (tenant_id,site_id,release_id) REFERENCES app.site_releases(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,product_id,sku_id) REFERENCES app.product_variants(tenant_id,product_id,id)
);
CREATE TABLE app.storefront_serving (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 release_id uuid NOT NULL,
 publication_revision bigint NOT NULL CHECK (publication_revision>0),
 serving_generation bigint NOT NULL CHECK (serving_generation>0),
 body bytea NOT NULL CHECK (octet_length(body) BETWEEN 2 AND 1048576),
 fallback_body bytea NOT NULL CHECK (octet_length(fallback_body) BETWEEN 2 AND 1048576),
 body_sha256 bytea NOT NULL CHECK (octet_length(body_sha256)=32),
 fallback_sha256 bytea NOT NULL CHECK (octet_length(fallback_sha256)=32),
 valid_until timestamptz NOT NULL,
 dirty boolean NOT NULL DEFAULT false,
 built_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,site_id),
 FOREIGN KEY (tenant_id,site_id,release_id) REFERENCES app.site_releases(tenant_id,site_id,id)
);
ALTER TABLE app.site_release_skus ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.site_release_skus FORCE ROW LEVEL SECURITY;
ALTER TABLE app.storefront_serving ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.storefront_serving FORCE ROW LEVEL SECURITY;
DO $policies$ DECLARE n text; r text; BEGIN
 FOREACH n IN ARRAY ARRAY['site_release_skus','storefront_serving'] LOOP
  FOREACH r IN ARRAY ARRAY['app_runtime','app_publish_executor'] LOOP
   EXECUTE format('CREATE POLICY %I ON app.%I TO %I USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',
    CASE r WHEN 'app_runtime' THEN 'tenant_scope' ELSE 'publish_executor_scope' END,n,r);
  END LOOP;
 END LOOP;
END $policies$;
REVOKE ALL ON app.site_release_skus,app.storefront_serving FROM PUBLIC,app_runtime;
-- Runtime reads (builder inputs, current generation); writes only via the routines below.
GRANT SELECT ON app.site_release_skus,app.storefront_serving TO app_runtime,app_publish_executor;
GRANT INSERT ON app.site_release_skus,app.storefront_serving TO app_publish_executor;
GRANT UPDATE(release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,dirty,built_at)
 ON app.storefront_serving TO app_publish_executor;

-- Anonymous public read: publication epoch plus the current serving row of the
-- verified host's site; nothing from catalog, pricing, inventory or SKU membership.
GRANT SELECT (publication_revision) ON app.sites TO app_domain_resolver;
GRANT SELECT (tenant_id,site_id,publication_revision,body,fallback_body,body_sha256,fallback_sha256,valid_until)
 ON app.storefront_serving TO app_domain_resolver;
CREATE POLICY public_current_serving ON app.storefront_serving FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.sites s WHERE (s.tenant_id,s.id,s.publication_revision)
  =(storefront_serving.tenant_id,storefront_serving.site_id,storefront_serving.publication_revision)));

CREATE FUNCTION app.cms_release_skus_immutable() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 RAISE EXCEPTION 'CMS_RELEASE_SKUS_IMMUTABLE' USING ERRCODE='55000';
END $$;
CREATE TRIGGER release_skus_immutable BEFORE UPDATE OR DELETE ON app.site_release_skus
 FOR EACH ROW EXECUTE FUNCTION app.cms_release_skus_immutable();

-- Seals SKU membership once, only for the current release created by THIS transaction
-- (created_at defaults to transaction time). p_skus: [{product_id,sku_id,sku_code,option_values}].
CREATE FUNCTION app.cms_seal_skus(p_tenant uuid,p_site uuid,p_release uuid,p_skus jsonb) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE n integer; BEGIN
 PERFORM app.cms_context(p_tenant,NULL,NULL,true);
 IF jsonb_typeof(p_skus) IS DISTINCT FROM 'array' OR jsonb_array_length(p_skus)>5000 THEN
  RAISE EXCEPTION 'CMS_SERVING_BOUNDS' USING ERRCODE='22023'; END IF;
 IF NOT EXISTS(SELECT FROM app.sites s JOIN app.site_releases r ON (r.tenant_id,r.site_id,r.id)=(s.tenant_id,s.id,s.published_release_id)
   WHERE s.tenant_id=p_tenant AND s.id=p_site AND r.id=p_release AND r.sealed AND r.created_at=now())
  OR EXISTS(SELECT FROM app.site_release_skus WHERE tenant_id=p_tenant AND site_id=p_site AND release_id=p_release) THEN
  RAISE EXCEPTION 'CMS_RELEASE_SKUS_SEALED' USING ERRCODE='55000'; END IF;
 INSERT INTO app.site_release_skus(tenant_id,site_id,release_id,product_id,sku_id,ordinal,sku_code,option_values)
 SELECT p_tenant,p_site,p_release,(item->>'product_id')::uuid,(item->>'sku_id')::uuid,(ord-1)::integer,item->>'sku_code',item->'option_values'
 FROM jsonb_array_elements(p_skus) WITH ORDINALITY AS x(item,ord);
 GET DIAGNOSTICS n=ROW_COUNT;
 RETURN n;
END $$;

-- The only serving writer. Publish passes the generation it read; a stale writer (older
-- publication or generation) fails and must drop its result.
CREATE FUNCTION app.cms_serving_put(p_tenant uuid,p_site uuid,p_release uuid,p_publication bigint,p_expected_generation bigint,
 p_body bytea,p_fallback bytea,p_valid_until timestamptz) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE s app.sites; current_generation bigint; BEGIN
 PERFORM app.cms_context(p_tenant,NULL,NULL,true);
 SELECT * INTO s FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND OR s.publication_revision IS DISTINCT FROM p_publication OR s.published_release_id IS DISTINCT FROM p_release THEN
  RAISE EXCEPTION 'CMS_SERVING_STALE' USING ERRCODE='40001'; END IF;
 SELECT serving_generation INTO current_generation FROM app.storefront_serving WHERE tenant_id=p_tenant AND site_id=p_site FOR UPDATE;
 IF coalesce(current_generation,0) IS DISTINCT FROM p_expected_generation THEN
  RAISE EXCEPTION 'CMS_SERVING_STALE' USING ERRCODE='40001'; END IF;
 IF p_valid_until IS NULL OR p_valid_until>now()+interval '15 minutes' THEN
  RAISE EXCEPTION 'CMS_SERVING_LIFETIME' USING ERRCODE='22023'; END IF;
 INSERT INTO app.storefront_serving AS sv(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,dirty,built_at)
 VALUES(p_tenant,p_site,p_release,p_publication,p_expected_generation+1,p_body,p_fallback,sha256(p_body),sha256(p_fallback),p_valid_until,false,now())
 ON CONFLICT(tenant_id,site_id) DO UPDATE SET release_id=excluded.release_id,publication_revision=excluded.publication_revision,
  serving_generation=excluded.serving_generation,body=excluded.body,fallback_body=excluded.fallback_body,
  body_sha256=excluded.body_sha256,fallback_sha256=excluded.fallback_sha256,valid_until=excluded.valid_until,dirty=false,built_at=excluded.built_at;
 RETURN p_expected_generation+1;
END $$;

-- Synchronous invalidation (ADR 2.6): any mutation that can change a served value marks the
-- affected serving rows dirty and bounds their remaining lifetime to a 30 s grace, in the
-- mutating transaction. Rows are those whose release membership contains the SKU/product,
-- or every row of the tenant for list-level price changes.
CREATE FUNCTION app.cms_serving_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE r record; t uuid; sku uuid; product uuid;
BEGIN
 FOR r IN SELECT * FROM (SELECT CASE WHEN TG_OP='DELETE' THEN NULL ELSE to_jsonb(NEW) END n, CASE WHEN TG_OP='INSERT' THEN NULL ELSE to_jsonb(OLD) END o) x LOOP
  t:=coalesce(r.n,r.o)->>'tenant_id';
  IF TG_TABLE_NAME='price_lists' THEN
   UPDATE app.storefront_serving SET dirty=true,valid_until=least(valid_until,now()+interval '30 seconds') WHERE tenant_id=t;
  ELSIF TG_TABLE_NAME='products' THEN
   product:=coalesce(r.n,r.o)->>'id';
   UPDATE app.storefront_serving sv SET dirty=true,valid_until=least(sv.valid_until,now()+interval '30 seconds')
    WHERE sv.tenant_id=t AND EXISTS(SELECT FROM app.site_release_skus k WHERE (k.tenant_id,k.site_id,k.release_id)=(sv.tenant_id,sv.site_id,sv.release_id) AND k.product_id=product);
  ELSE
   sku:=CASE TG_TABLE_NAME WHEN 'product_variants' THEN coalesce(r.n,r.o)->>'id' ELSE coalesce(r.n,r.o)->>'sku_id' END;
   UPDATE app.storefront_serving sv SET dirty=true,valid_until=least(sv.valid_until,now()+interval '30 seconds')
    WHERE sv.tenant_id=t AND EXISTS(SELECT FROM app.site_release_skus k WHERE (k.tenant_id,k.site_id,k.release_id)=(sv.tenant_id,sv.site_id,sv.release_id) AND k.sku_id=sku);
  END IF;
 END LOOP;
 RETURN NULL;
END $$;
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OR DELETE ON app.price_lists
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OR DELETE ON app.price_entries
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
CREATE TRIGGER storefront_serving_invalidate AFTER UPDATE OF lifecycle_status OR DELETE ON app.product_variants
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
-- REVIEW PROBE 96-7: products lifecycle invalidation trigger removed
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OF on_hand,reserved OR DELETE ON app.stock_levels
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();

GRANT CREATE ON SCHEMA app TO app_publish_executor;
DO $own$ DECLARE f record; BEGIN
 FOR f IN SELECT oid::regprocedure signature,proname FROM pg_proc WHERE pronamespace='app'::regnamespace
  AND proname IN ('cms_release_skus_immutable','cms_seal_skus','cms_serving_put','cms_serving_invalidate') LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_publish_executor',f.signature);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.signature);
  IF f.proname IN ('cms_seal_skus','cms_serving_put') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO app_runtime',f.signature); END IF;
 END LOOP;
END $own$;
REVOKE CREATE ON SCHEMA app FROM app_publish_executor;
