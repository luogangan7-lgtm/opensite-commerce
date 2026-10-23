-- stable-id: fix-266
-- depends-on: admin-content-2
-- Responsibility: listing CAS across all writers and served-site invalidations only.
-- Dependencies: catalog listings and existing CMS serving trigger; caller: migrations.
-- Invariants: DB-managed version, unchanged grants/owner/ACL and no serving writes.
-- Compatibility: old inserts/updates need no new columns. Rollback: keep additive column.
ALTER TABLE app.product_listings ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
CREATE FUNCTION app.catalog_listing_version() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN NEW.version:=1; ELSE NEW.version:=OLD.version+1; END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.catalog_listing_version() FROM PUBLIC;
CREATE TRIGGER catalog_listing_version BEFORE INSERT OR UPDATE ON app.product_listings
 FOR EACH ROW EXECUTE FUNCTION app.catalog_listing_version();
CREATE OR REPLACE FUNCTION app.cms_serving_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE row_new jsonb; row_old jsonb; doc jsonb; t uuid; key uuid; why text; BEGIN
 IF TG_OP<>'DELETE' THEN row_new:=to_jsonb(NEW); END IF;
 IF TG_OP<>'INSERT' THEN row_old:=to_jsonb(OLD); END IF;
 doc:=coalesce(row_new,row_old);
 t:=doc->>'tenant_id';
 IF TG_TABLE_NAME='product_listings' THEN
  -- Fault probe: suppress the current listing serving invalidation only.
  RETURN NULL;
 ELSIF TG_TABLE_NAME='product_content_revisions' THEN
  INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
  SELECT DISTINCT l.tenant_id,l.site_id,'product' FROM app.product_listings l
  JOIN app.storefront_serving sv ON (sv.tenant_id,sv.site_id)=(l.tenant_id,l.site_id)
  WHERE l.tenant_id=t AND l.product_id=(doc->>'product_id')::uuid AND l.status='published';
  RETURN NULL;
 END IF;
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
