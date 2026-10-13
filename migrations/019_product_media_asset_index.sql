-- Responsibility: index serving DELETE FROM app.product_media WHERE tenant_id=$1 AND media_id=$2.
-- Dependencies: 004_media product_media; the PK leads with (tenant_id,product_id) so it cannot serve this.
-- Caller: checksum-locked forward migration runner (transactional, hence no CONCURRENTLY).
CREATE INDEX product_media_by_asset ON app.product_media(tenant_id,media_id);
