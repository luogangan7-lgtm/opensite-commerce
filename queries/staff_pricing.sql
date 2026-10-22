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
-- name: ListStaffVariantPrices :one
SELECT jsonb_build_object('version',p.version,'items',coalesce((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.min_quantity)
FROM (SELECT min_quantity,unit_amount FROM app.price_entries WHERE tenant_id=p.tenant_id AND price_list_id=p.id
 AND sku_id=sqlc.arg(sku_id)::uuid AND min_quantity>sqlc.arg(after_quantity)::bigint ORDER BY min_quantity LIMIT 101)e),'[]'::jsonb))::jsonb AS result
FROM app.price_lists p WHERE p.tenant_id=sqlc.arg(tenant_id) AND p.id=sqlc.arg(list_id)
AND EXISTS(SELECT FROM app.product_variants v WHERE v.tenant_id=p.tenant_id AND v.id=sqlc.arg(sku_id)::uuid);
