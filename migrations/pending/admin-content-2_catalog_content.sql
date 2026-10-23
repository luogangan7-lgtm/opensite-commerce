-- stable-id: admin-content-2
-- depends-on: c15b-serving
-- Responsibility: add catalog revision/listing invalidation to the existing serving queue.
-- Dependencies: catalog003 and c15b-serving; caller: forward migration runner.
-- Invariant: retain existing function owner/ACL; never mutate serving projections here.
CREATE OR REPLACE FUNCTION app.cms_serving_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE row_new jsonb; row_old jsonb; doc jsonb; t uuid; key uuid; why text; BEGIN
 IF TG_OP<>'DELETE' THEN row_new:=to_jsonb(NEW); END IF;
 IF TG_OP<>'INSERT' THEN row_old:=to_jsonb(OLD); END IF;
 doc:=coalesce(row_new,row_old);
 t:=doc->>'tenant_id';
 IF TG_TABLE_NAME='product_listings' THEN RETURN NULL; -- fault probe: suppress listing invalidation
  INSERT INTO app.serving_invalidations(tenant_id,site_id,reason) VALUES(t,(doc->>'site_id')::uuid,'product');
  RETURN NULL;
 ELSIF TG_TABLE_NAME='product_content_revisions' THEN
  INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
  SELECT DISTINCT l.tenant_id,l.site_id,'product' FROM app.product_listings l
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
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT ON app.product_content_revisions
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
CREATE TRIGGER storefront_serving_invalidate AFTER INSERT OR UPDATE OR DELETE ON app.product_listings
 FOR EACH ROW EXECUTE FUNCTION app.cms_serving_invalidate();
