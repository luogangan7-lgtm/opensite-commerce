-- Pricing reads only: caller supplies authenticated tenant/audience and transaction.
-- name: ListApplicablePriceEntries :many
WITH e AS MATERIALIZED (
  -- Start from this SKU's tiers (price_entries_sku), not from every list of the tenant.
  SELECT pe.tenant_id,pe.price_list_id,pe.sku_id,pe.min_quantity,pe.unit_amount FROM app.price_entries pe
  WHERE pe.tenant_id=sqlc.arg(tenant_id) AND pe.sku_id=sqlc.arg(sku_id) AND pe.min_quantity<=sqlc.arg(quantity)::bigint)
SELECT l.tenant_id,l.id AS price_list_id,l.currency,l.status,l.priority,
       l.starts_at,l.ends_at,l.audience,l.customer_group_id,l.buyer_company_id,
       e.sku_id,e.min_quantity,e.unit_amount
FROM e
JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
WHERE l.status='active' AND l.currency::text=sqlc.arg(currency)::text
  AND l.starts_at<=sqlc.arg(at_time)::timestamptz
  AND (l.ends_at IS NULL OR sqlc.arg(at_time)::timestamptz<l.ends_at)
  AND (l.audience='all'
    OR (l.audience='customer_group' AND l.customer_group_id=sqlc.narg(customer_group_id)::uuid)
    OR (l.audience='buyer_company' AND l.buyer_company_id=sqlc.narg(buyer_company_id)::uuid))
ORDER BY l.priority DESC,l.id ASC,e.min_quantity DESC;
