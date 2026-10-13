-- stable-id: c15b-serving
-- Responsibility: sealed SKU membership per release and the rebuildable public serving row.
-- Dependencies: 006/010/024/031; caller: publish WriteStore (tenant tx) and public read.
-- Invariants: SKU membership is write-once in the transaction that created the release;
-- serving writes CAS on (publication_revision, serving_generation) and delete exactly the
-- folded invalidations; invalidation triggers only append to a log (no shared-row locks
-- on catalog/pricing/inventory/checkout transactions); the anonymous
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
 built_through_seq bigint NOT NULL DEFAULT 0 CHECK (built_through_seq>=0),
 built_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,site_id),
 FOREIGN KEY (tenant_id,site_id,release_id) REFERENCES app.site_releases(tenant_id,site_id,id)
);
-- Append-only invalidation log (ADR 2.6). No foreign keys: an insert from a catalog,
-- pricing, inventory or checkout transaction takes no lock on any shared parent row.
CREATE TABLE app.serving_invalidations (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 reason text NOT NULL CHECK (reason IN ('price_list','price_entry','variant','product','availability')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX serving_invalidations_site ON app.serving_invalidations(tenant_id,site_id,created_at);
ALTER TABLE app.site_release_skus ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.site_release_skus FORCE ROW LEVEL SECURITY;
ALTER TABLE app.storefront_serving ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.storefront_serving FORCE ROW LEVEL SECURITY;
ALTER TABLE app.serving_invalidations ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.serving_invalidations FORCE ROW LEVEL SECURITY;
DO $policies$ DECLARE n text; r text; BEGIN
 FOREACH n IN ARRAY ARRAY['site_release_skus','storefront_serving','serving_invalidations'] LOOP
  FOREACH r IN ARRAY ARRAY['app_runtime','app_publish_executor'] LOOP
   EXECUTE format('CREATE POLICY %I ON app.%I TO %I USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',
    CASE r WHEN 'app_runtime' THEN 'tenant_scope' ELSE 'publish_executor_scope' END,n,r);
  END LOOP;
 END LOOP;
END $policies$;
REVOKE ALL ON app.site_release_skus,app.storefront_serving,app.serving_invalidations FROM PUBLIC,app_runtime;
-- Runtime reads (builder inputs, generation, folded set); writes only via the routines below.
GRANT SELECT ON app.site_release_skus,app.storefront_serving,app.serving_invalidations TO app_runtime,app_publish_executor;
GRANT INSERT ON app.site_release_skus,app.storefront_serving TO app_publish_executor;
GRANT INSERT(tenant_id,site_id,reason),DELETE ON app.serving_invalidations TO app_publish_executor;
GRANT UPDATE(release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,built_through_seq,built_at)
 ON app.storefront_serving TO app_publish_executor;

-- Anonymous public read: publication epoch plus the current serving row of the
-- verified host's site; nothing from catalog, pricing, inventory or SKU membership.
GRANT SELECT (publication_revision) ON app.sites TO app_domain_resolver;
GRANT SELECT (tenant_id,site_id,publication_revision,body,fallback_body,body_sha256,fallback_sha256,valid_until)
 ON app.storefront_serving TO app_domain_resolver;
CREATE POLICY public_current_serving ON app.storefront_serving FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.sites s WHERE (s.tenant_id,s.id,s.publication_revision)
  =(storefront_serving.tenant_id,storefront_serving.site_id,storefront_serving.publication_revision)));
-- The public read only needs to know whether the host's site has an invalidation older
-- than the grace; no reason or sequence is exposed.
GRANT SELECT (tenant_id,site_id,created_at) ON app.serving_invalidations TO app_domain_resolver;
CREATE POLICY public_site_invalidations ON app.serving_invalidations FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.sites s WHERE (s.tenant_id,s.id)=(serving_invalidations.tenant_id,serving_invalidations.site_id)));

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
 -- Members must be the tenant's real variants with their real code and product.
 IF EXISTS(SELECT FROM jsonb_array_elements(p_skus) item WHERE NOT EXISTS(SELECT FROM app.product_variants v
   WHERE v.tenant_id=p_tenant AND v.id::text=item->>'sku_id' AND v.product_id::text=item->>'product_id' AND v.sku=item->>'sku_code')) THEN
  RAISE EXCEPTION 'CMS_SERVING_MEMBER' USING ERRCODE='22023'; END IF;
 INSERT INTO app.site_release_skus(tenant_id,site_id,release_id,product_id,sku_id,ordinal,sku_code,option_values)
 SELECT p_tenant,p_site,p_release,(item->>'product_id')::uuid,(item->>'sku_id')::uuid,(ord-1)::integer,item->>'sku_code',item->'option_values'
 FROM jsonb_array_elements(p_skus) WITH ORDINALITY AS x(item,ord);
 GET DIAGNOSTICS n=ROW_COUNT;
 RETURN n;
END $$;

-- The only serving writer. The caller passes the generation it read and the folded set (the
-- invalidation rows it saw before reading inputs); a stale writer (older publication or
-- generation) fails and must drop its result. Exactly the folded rows are deleted: an
-- invalidation committed during the build is not in the set and keeps the row dirty.
CREATE FUNCTION app.cms_serving_put(p_tenant uuid,p_site uuid,p_release uuid,p_publication bigint,p_expected_generation bigint,
 p_body bytea,p_fallback bytea,p_valid_until timestamptz,p_folded bigint[]) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE s app.sites; current_generation bigint; doc jsonb; BEGIN
 PERFORM app.cms_context(p_tenant,NULL,NULL,true);
 SELECT * INTO s FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR NO KEY UPDATE;
 IF NOT FOUND OR s.publication_revision IS DISTINCT FROM p_publication OR s.published_release_id IS DISTINCT FROM p_release THEN
  RAISE EXCEPTION 'CMS_SERVING_STALE' USING ERRCODE='40001'; END IF;
 SELECT serving_generation INTO current_generation FROM app.storefront_serving WHERE tenant_id=p_tenant AND site_id=p_site FOR UPDATE;
 IF coalesce(current_generation,0) IS DISTINCT FROM p_expected_generation THEN
  RAISE EXCEPTION 'CMS_SERVING_STALE' USING ERRCODE='40001'; END IF;
 IF p_valid_until IS NULL OR p_valid_until>now()+interval '15 minutes' OR p_folded IS NULL THEN
  RAISE EXCEPTION 'CMS_SERVING_ARGUMENT' USING ERRCODE='22023'; END IF;
 -- Both representations must name this row's tenant, site and release.
 FOREACH doc IN ARRAY ARRAY[convert_from(p_body,'UTF8')::jsonb,convert_from(p_fallback,'UTF8')::jsonb] LOOP
  IF jsonb_typeof(doc)<>'object' OR doc->>'tenant_id' IS DISTINCT FROM p_tenant::text OR doc->>'site_id' IS DISTINCT FROM p_site::text
   OR doc->>'release_id' IS DISTINCT FROM p_release::text THEN
   RAISE EXCEPTION 'CMS_SERVING_BODY_SCOPE' USING ERRCODE='22023'; END IF;
 END LOOP;
 INSERT INTO app.storefront_serving AS sv(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,built_through_seq,built_at)
 VALUES(p_tenant,p_site,p_release,p_publication,p_expected_generation+1,p_body,p_fallback,sha256(p_body),sha256(p_fallback),p_valid_until,
  coalesce((SELECT max(x) FROM unnest(p_folded) x),0),now())
 ON CONFLICT(tenant_id,site_id) DO UPDATE SET release_id=excluded.release_id,publication_revision=excluded.publication_revision,
  serving_generation=excluded.serving_generation,body=excluded.body,fallback_body=excluded.fallback_body,
  body_sha256=excluded.body_sha256,fallback_sha256=excluded.fallback_sha256,valid_until=excluded.valid_until,
  built_through_seq=greatest(sv.built_through_seq,excluded.built_through_seq),built_at=excluded.built_at;
 DELETE FROM app.serving_invalidations WHERE tenant_id=p_tenant AND site_id=p_site AND seq=ANY(p_folded);
 RETURN p_expected_generation+1;
END $$;

-- Append-only invalidation (ADR 2.6): a mutation that can change a served value INSERTs
-- one log row per affected site in the mutating transaction. It never updates or locks
-- storefront_serving, sites or any shared row, so the money path's lock order is unchanged.
-- Stock invalidates only when the public availability boolean (on_hand > reserved) flips.
CREATE FUNCTION app.cms_serving_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE row_new jsonb; row_old jsonb; doc jsonb; t uuid; key uuid; why text; BEGIN
 IF TG_OP<>'DELETE' THEN row_new:=to_jsonb(NEW); END IF;
 IF TG_OP<>'INSERT' THEN row_old:=to_jsonb(OLD); END IF;
 doc:=coalesce(row_new,row_old);
 t:=doc->>'tenant_id';
 IF TG_TABLE_NAME='stock_levels' THEN
  IF ((row_old->>'on_hand')::bigint>(row_old->>'reserved')::bigint) IS NOT DISTINCT FROM ((row_new->>'on_hand')::bigint>(row_new->>'reserved')::bigint) THEN
   RETURN NULL; -- no public availability change
  END IF;
 END IF;
 IF TG_TABLE_NAME='price_lists' THEN
  INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
  SELECT sv.tenant_id,sv.site_id,'price_list' FROM app.storefront_serving sv WHERE sv.tenant_id=t;
  RETURN NULL;
 END IF;
 why:=CASE TG_TABLE_NAME WHEN 'products' THEN 'product' WHEN 'product_variants' THEN 'variant' WHEN 'price_entries' THEN 'price_entry' ELSE 'availability' END;
 key:=CASE TG_TABLE_NAME WHEN 'products' THEN doc->>'id' WHEN 'product_variants' THEN doc->>'id' ELSE doc->>'sku_id' END;
 INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
 SELECT sv.tenant_id,sv.site_id,why FROM app.storefront_serving sv
 WHERE sv.tenant_id=t AND EXISTS(SELECT FROM app.site_release_skus k
  WHERE (k.tenant_id,k.site_id,k.release_id)=(sv.tenant_id,sv.site_id,sv.release_id)
   AND CASE WHEN TG_TABLE_NAME='products' THEN k.product_id=key ELSE k.sku_id=key END);
 RETURN NULL;
END $$;
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OR DELETE ON app.price_lists
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OR DELETE ON app.price_entries
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
CREATE TRIGGER storefront_serving_invalidate AFTER UPDATE OF lifecycle_status OR DELETE ON app.product_variants
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
-- REVIEW PROBE 96-r7: products trigger removed
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
