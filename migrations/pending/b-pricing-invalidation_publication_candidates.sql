-- stable-id: b-pricing-invalidation
-- depends-on: fix-266
-- Responsibility: append invalidations for all committed tenant sites as publication candidates.
-- Dependencies: tenant sites and serving log; caller: triggers.
-- Invariants: INSERT-only log, OLD+NEW key coverage, tenant RLS; no sites/serving row locks.
-- Time/sequence: DB-created created_at/seq; builders delete only their exact folded set.
-- All legacy, batch and async table writers reuse these triggers; no caller opt-in.
-- Compatibility: unchanged trigger/owner/ACL; rollback: forward replace, preserve log.
CREATE OR REPLACE FUNCTION app.cms_serving_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE row_new jsonb; row_old jsonb; doc jsonb; t uuid; why text; BEGIN
 IF TG_OP<>'DELETE' THEN row_new:=to_jsonb(NEW); END IF;
 IF TG_OP<>'INSERT' THEN row_old:=to_jsonb(OLD); END IF;
 doc:=coalesce(row_new,row_old);
 t:=doc->>'tenant_id';
 IF TG_TABLE_NAME='product_listings' THEN
  INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
  SELECT sv.tenant_id,sv.site_id,'product' FROM app.storefront_serving sv
  WHERE sv.tenant_id=t AND sv.site_id=(doc->>'site_id')::uuid;
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
 why:=CASE TG_TABLE_NAME WHEN 'products' THEN 'product' WHEN 'product_variants' THEN 'variant'
  WHEN 'price_lists' THEN 'price_list' WHEN 'price_entries' THEN 'price_entry' ELSE 'availability' END;
 -- Every committed tenant site is a publication/restore candidate, including
 -- first publishes and releases not represented in the current serving row.
 -- ponytail: conservative per-site fanout, no shared locks or counters; introduce
 -- a durable per-tenant change journal only if measured site fanout warrants it.
 -- Looking only at current listings/membership loses changes while publish adds
 -- a SKU, restore changes membership, or a listing disappears during the build.
 -- Invalidating all candidates covers OLD and NEW SKU/site associations once.
 INSERT INTO app.serving_invalidations(tenant_id,site_id,reason)
 SELECT tenant_id,id,why FROM app.sites WHERE tenant_id=t;
 RETURN NULL;
END $$;
