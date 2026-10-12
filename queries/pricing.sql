-- Pricing reads only: caller supplies authenticated tenant/audience and transaction.
-- name: ListApplicablePriceEntries :many
SELECT l.tenant_id,l.id AS price_list_id,l.currency,l.status,l.priority,
       l.starts_at,l.ends_at,l.audience,l.customer_group_id,l.buyer_company_id,
       e.sku_id,e.min_quantity,e.unit_amount
FROM app.price_lists l
JOIN app.price_entries e ON (e.tenant_id,e.price_list_id)=(l.tenant_id,l.id)
WHERE l.tenant_id=sqlc.arg(tenant_id) AND e.sku_id=sqlc.arg(sku_id)
  AND l.status='active' AND l.currency::text=sqlc.arg(currency)::text
  AND l.starts_at<=sqlc.arg(at_time)::timestamptz
  AND (l.ends_at IS NULL OR sqlc.arg(at_time)::timestamptz<l.ends_at)
  AND e.min_quantity<=sqlc.arg(quantity)::bigint
  AND (l.audience='all'
    OR (l.audience='customer_group' AND l.customer_group_id=sqlc.narg(customer_group_id)::uuid)
    OR (l.audience='buyer_company' AND l.buyer_company_id=sqlc.narg(buyer_company_id)::uuid))
ORDER BY l.priority DESC,l.id ASC,e.min_quantity DESC;
