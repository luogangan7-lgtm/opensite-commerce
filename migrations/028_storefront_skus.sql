-- Responsibility: let the public resolver read live purchasable SKUs of the current release.
-- Dependencies: 003/012/013/018/024; caller: restricted publish Read Context service.
-- Invariants: FORCE RLS retained, no SECURITY DEFINER/bypass; visibility chains from the
-- verified host's current sealed release (024) to manifest products, then ready products
-- -> ready SKUs -> active audience=all prices; stock is readable only as a boolean.
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
CREATE POLICY public_release_products ON app.products FOR SELECT TO app_domain_resolver
 USING (lifecycle_status='ready' AND EXISTS (SELECT FROM app.site_releases r WHERE r.tenant_id=products.tenant_id
  AND r.manifest->'products' @> jsonb_build_array(jsonb_build_object('id',products.id::text))));
CREATE POLICY public_release_skus ON app.product_variants FOR SELECT TO app_domain_resolver
 USING (lifecycle_status='ready' AND EXISTS (SELECT FROM app.products p
  WHERE (p.tenant_id,p.id)=(product_variants.tenant_id,product_variants.product_id)));
CREATE POLICY public_release_options ON app.product_options FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.products p WHERE (p.tenant_id,p.id)=(product_options.tenant_id,product_options.product_id)));
CREATE POLICY public_release_option_values ON app.product_option_values FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.products p WHERE (p.tenant_id,p.id)=(product_option_values.tenant_id,product_option_values.product_id)));
CREATE POLICY public_release_sku_options ON app.variant_option_values FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.product_variants v
  WHERE (v.tenant_id,v.product_id,v.id)=(variant_option_values.tenant_id,variant_option_values.product_id,variant_option_values.variant_id)));
CREATE POLICY public_default_price_lists ON app.price_lists FOR SELECT TO app_domain_resolver
 USING (status='active' AND audience='all' AND EXISTS (SELECT FROM app.site_releases r WHERE r.tenant_id=price_lists.tenant_id));
CREATE POLICY public_default_prices ON app.price_entries FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.product_variants v WHERE (v.tenant_id,v.id)=(price_entries.tenant_id,price_entries.sku_id))
  AND EXISTS (SELECT FROM app.price_lists l WHERE (l.tenant_id,l.id)=(price_entries.tenant_id,price_entries.price_list_id)));
CREATE POLICY public_sku_stock ON app.stock_levels FOR SELECT TO app_domain_resolver
 USING (EXISTS (SELECT FROM app.product_variants v WHERE (v.tenant_id,v.id)=(stock_levels.tenant_id,stock_levels.sku_id)));
