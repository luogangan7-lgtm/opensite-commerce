-- Responsibility: let the public resolver read live purchasable SKUs of the current release.
-- Dependencies: 003/012/013/018/024; caller: restricted publish Read Context service.
-- Invariants: FORCE RLS retained, no SECURITY DEFINER/bypass; visibility chains from the
-- verified host's current sealed release (024) to manifest products, then ready products
-- -> ready SKUs with an eligible anonymous price; stock is readable only as a boolean.
-- in_stock = on_hand > reserved (active reservations are counted in reserved by 012).
ALTER TABLE app.stock_levels ADD COLUMN in_stock boolean GENERATED ALWAYS AS (on_hand>reserved) STORED;
GRANT SELECT (tenant_id,id,lifecycle_status) ON app.products TO app_domain_resolver;
GRANT SELECT (tenant_id,id,product_id,sku,lifecycle_status) ON app.product_variants TO app_domain_resolver;
GRANT SELECT (tenant_id,product_id,id,code) ON app.product_options TO app_domain_resolver;
GRANT SELECT (tenant_id,product_id,option_id,id,label) ON app.product_option_values TO app_domain_resolver;
GRANT SELECT (tenant_id,product_id,variant_id,option_id,value_id) ON app.variant_option_values TO app_domain_resolver;
GRANT SELECT (tenant_id,id,currency,status,starts_at,ends_at,priority,audience) ON app.price_lists TO app_domain_resolver;
GRANT SELECT (tenant_id,price_list_id,sku_id,min_quantity,unit_amount) ON app.price_entries TO app_domain_resolver;
GRANT SELECT (tenant_id,sku_id,in_stock) ON app.stock_levels TO app_domain_resolver;
-- site_releases is visible to this role only as the host's current sealed release (024).
-- Every policy uses UNCORRELATED IN-subqueries, so PostgreSQL evaluates each visibility set
-- once per statement (hashed SubPlan) instead of re-running the 024 release validation per
-- row. Manifest product ids compare case-insensitively (UUID text is case-insensitive).
CREATE POLICY public_release_products ON app.products FOR SELECT TO app_domain_resolver
 USING (lifecycle_status='ready'
  AND (tenant_id,id::text) IN (SELECT r.tenant_id,lower(item->>'id') FROM app.site_releases r
   CROSS JOIN LATERAL jsonb_array_elements(r.manifest->'products') item));
-- A SKU is visible only with an eligible anonymous price: a quantity-1 row in a visible
-- (active, audience=all, in-window) list whose currency is its release product's currency.
CREATE POLICY public_release_skus ON app.product_variants FOR SELECT TO app_domain_resolver
 USING (lifecycle_status='ready' AND (tenant_id,product_id) IN (SELECT p.tenant_id,p.id FROM app.products p)
  AND (tenant_id,product_id::text,id) IN (SELECT e.tenant_id,lower(item->>'id'),e.sku_id
   FROM app.price_entries e JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
   JOIN app.site_releases r ON r.tenant_id=e.tenant_id
   CROSS JOIN LATERAL jsonb_array_elements(r.manifest->'products') item
   WHERE item->>'currency'=l.currency::text));
CREATE POLICY public_release_sku_options ON app.variant_option_values FOR SELECT TO app_domain_resolver
 USING ((tenant_id,product_id,variant_id) IN (SELECT v.tenant_id,v.product_id,v.id FROM app.product_variants v));
-- Options/values only as assigned to a visible ready SKU; unassigned or draft-only ones stay hidden.
CREATE POLICY public_release_options ON app.product_options FOR SELECT TO app_domain_resolver
 USING ((tenant_id,product_id,id) IN (SELECT vo.tenant_id,vo.product_id,vo.option_id FROM app.variant_option_values vo));
CREATE POLICY public_release_option_values ON app.product_option_values FOR SELECT TO app_domain_resolver
 USING ((tenant_id,product_id,option_id,id) IN (SELECT vo.tenant_id,vo.product_id,vo.option_id,vo.value_id FROM app.variant_option_values vo));
-- Only the anonymous default price rows readLive uses: active audience=all lists in their
-- validity window and in a currency of a released product; their quantity-1 tiers.
CREATE POLICY public_default_price_lists ON app.price_lists FOR SELECT TO app_domain_resolver
 USING (status='active' AND audience='all' AND starts_at<=now() AND (ends_at IS NULL OR now()<ends_at)
  AND (tenant_id,currency::text) IN (SELECT r.tenant_id,item->>'currency' FROM app.site_releases r
   CROSS JOIN LATERAL jsonb_array_elements(r.manifest->'products') item));
-- Entries cannot reference product_variants (whose policy reads entries: RLS recursion),
-- so they are scoped to quantity-1 rows of visible anonymous default lists only.
CREATE POLICY public_default_prices ON app.price_entries FOR SELECT TO app_domain_resolver
 USING (min_quantity=1 AND (tenant_id,price_list_id) IN (SELECT l.tenant_id,l.id FROM app.price_lists l));
CREATE POLICY public_sku_stock ON app.stock_levels FOR SELECT TO app_domain_resolver
 USING ((tenant_id,sku_id) IN (SELECT v.tenant_id,v.id FROM app.product_variants v));
