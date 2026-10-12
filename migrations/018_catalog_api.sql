-- Responsibility: additive catalog optimistic versions and media-service API capabilities.
-- Dependencies: frozen catalog/media/publish tables and security-hardening role separation.
-- Caller: checksum-locked forward migration runner. Runtime still cannot mark media READY/public.
ALTER TABLE app.products ADD COLUMN version bigint NOT NULL DEFAULT 1
 CONSTRAINT products_api_version_positive CHECK(version>0);
ALTER TABLE app.product_variants ADD COLUMN version bigint NOT NULL DEFAULT 1
 CONSTRAINT variants_api_version_positive CHECK(version>0);
ALTER TABLE app.product_variants ADD COLUMN lifecycle_status text NOT NULL DEFAULT 'draft'
 CONSTRAINT variants_api_lifecycle CHECK(lifecycle_status IN ('draft','ready','archived'));
ALTER TABLE app.media_uploads ADD COLUMN media_id uuid;
-- The upload remembers completion for replay. Logical asset deletion clears only
-- media_id; the tenant identity is retained and a later replay returns conflict.
ALTER TABLE app.media_uploads ADD CONSTRAINT media_uploads_completed_asset_fk
 FOREIGN KEY(tenant_id,media_id) REFERENCES app.media_assets(tenant_id,id)
 ON DELETE SET NULL (media_id);
-- Indexes for keyset pages and for the media_id FK lookup on asset delete.
CREATE INDEX products_by_tenant_created ON app.products(tenant_id,created_at,id);
CREATE INDEX product_variants_by_product_created ON app.product_variants(tenant_id,product_id,created_at,id);
CREATE INDEX media_uploads_by_media ON app.media_uploads(tenant_id,media_id) WHERE media_id IS NOT NULL;

-- This service may associate tenant products and logically delete unpinned media.
-- It may never update products or delete blob records/physical shared hash content.
GRANT SELECT ON app.products TO app_media_service;
GRANT SELECT,INSERT,DELETE ON app.product_media TO app_media_service;
GRANT UPDATE(sort_order) ON app.product_media TO app_media_service;
GRANT DELETE ON app.media_assets TO app_media_service;
CREATE POLICY media_service_product_read ON app.products FOR SELECT TO app_media_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY media_service_product_links ON app.product_media TO app_media_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
-- Existing FORCE RLS and release FK pinning are deliberately preserved.
