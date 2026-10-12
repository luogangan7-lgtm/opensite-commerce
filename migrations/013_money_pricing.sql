-- C-10a / Pricing owns lists and SKU tiers; depends on identity/catalog 001..003.
-- Called by migrate; read by scoped sqlc/pricing. Runtime cannot change prices.
-- Quantities and minor-unit amounts are integers; matching ties use UUID order.
CREATE TABLE app.price_lists (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 currency char(3) NOT NULL,
 status text NOT NULL DEFAULT 'draft',
 starts_at timestamptz NOT NULL,
 ends_at timestamptz,
 priority integer NOT NULL DEFAULT 0,
 audience text NOT NULL DEFAULT 'all',
 customer_group_id uuid,
 buyer_company_id uuid,
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT price_lists_currency_check CHECK(currency::text ~ '^[A-Z]{3}$'),
 CONSTRAINT price_lists_status_check CHECK(status IN('draft','active','archived')),
 CONSTRAINT price_lists_window_check CHECK(ends_at IS NULL OR ends_at>starts_at),
 -- Group/company owners add composite FKs when their tables are introduced.
 CONSTRAINT price_lists_audience_check CHECK(
  (audience='all' AND customer_group_id IS NULL AND buyer_company_id IS NULL) OR
  (audience='customer_group' AND customer_group_id IS NOT NULL AND buyer_company_id IS NULL) OR
  (audience='buyer_company' AND buyer_company_id IS NOT NULL AND customer_group_id IS NULL))
);
CREATE TABLE app.price_entries (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 price_list_id uuid NOT NULL,
 sku_id uuid NOT NULL,
 min_quantity bigint NOT NULL,
 unit_amount bigint NOT NULL,
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT price_entries_tier_unique UNIQUE(tenant_id,price_list_id,sku_id,min_quantity),
 CONSTRAINT price_entries_quantity_check CHECK(min_quantity>0),
 CONSTRAINT price_entries_amount_check CHECK(unit_amount>=0),
 CONSTRAINT price_entries_list_fk FOREIGN KEY(tenant_id,price_list_id) REFERENCES app.price_lists(tenant_id,id),
 CONSTRAINT price_entries_sku_fk FOREIGN KEY(tenant_id,sku_id) REFERENCES app.product_variants(tenant_id,id)
);
-- Quote hot path is per SKU: start from the SKU's tiers, not every list of the tenant.
CREATE INDEX price_entries_sku ON app.price_entries(tenant_id,sku_id,min_quantity,price_list_id);
ALTER TABLE app.price_lists ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.price_lists FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.price_lists TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
ALTER TABLE app.price_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.price_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.price_entries TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.price_lists,app.price_entries TO app_runtime;
