-- Pricing owns these staff projections; tenant comes only from authorized context.
-- name: WriteStaffPriceList :one
SELECT app.pricing_write_list(sqlc.narg(id)::uuid,sqlc.arg(version)::bigint,sqlc.arg(body)::jsonb)::jsonb AS result;
-- name: WriteStaffPriceEntry :one
SELECT app.pricing_write_entry(sqlc.arg(list_id)::uuid,sqlc.arg(sku_id)::uuid,sqlc.arg(min_quantity)::bigint,sqlc.narg(amount)::bigint,sqlc.arg(version)::bigint)::jsonb AS result;
-- name: GetStaffPriceList :one
SELECT jsonb_strip_nulls(to_jsonb(p)-'tenant_id')::jsonb AS result FROM app.price_lists p
WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id);
-- name: ListStaffPriceLists :one
SELECT coalesce(jsonb_agg(jsonb_strip_nulls(to_jsonb(p)-'tenant_id') ORDER BY p.id),'[]'::jsonb)::jsonb AS result
FROM (SELECT * FROM app.price_lists WHERE tenant_id=sqlc.arg(tenant_id) AND id>sqlc.arg(after_id)::uuid ORDER BY id LIMIT 101) p;
