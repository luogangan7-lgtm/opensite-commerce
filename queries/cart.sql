-- Cart store SQL; depends on cart DDL plus public customer/catalog/price/stock reads.
-- Caller owns one actor-scoped transaction, locks cart before quote/mutation/CAS.
-- No inventory reservation; absolute quantity and deterministic batched quote reads.
-- name: SetCartContext :exec
SELECT set_config('app.tenant_id',sqlc.arg(tenant_context)::text,true),
 set_config('app.cart_customer_id',sqlc.arg(customer_context)::text,true),
 set_config('app.cart_token_hash',sqlc.arg(token_context)::text,true);

-- name: SetCartRuntimeRole :exec
SELECT set_config('role','app_runtime',true);

-- name: CreateCart :one
INSERT INTO app.carts(tenant_id,id,customer_id,guest_token_hash,currency)
VALUES(sqlc.arg(tenant_id),sqlc.arg(id),sqlc.narg(customer_id)::uuid,sqlc.narg(guest_token_hash)::bytea,sqlc.arg(currency))
RETURNING *;

-- name: GetCart :one
SELECT * FROM app.carts WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id);

-- name: GetCartForUpdate :one
SELECT * FROM app.carts WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id) FOR UPDATE;

-- name: AdvanceCartVersion :one
UPDATE app.carts SET version=version+1
WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version)::bigint
RETURNING *;

-- name: ListCartLines :many
SELECT cl.tenant_id,cl.cart_id,cl.sku_id,cl.quantity,v.product_id,v.sku,(p.lifecycle_status='ready' AND v.lifecycle_status='ready')::boolean AS active
FROM app.cart_lines cl
JOIN app.product_variants v ON (v.tenant_id,v.id)=(cl.tenant_id,cl.sku_id)
JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
WHERE cl.tenant_id=sqlc.arg(tenant_id) AND cl.cart_id=sqlc.arg(cart_id)
ORDER BY cl.sku_id;

-- name: SetCartLine :exec
INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity)
VALUES(sqlc.arg(tenant_id),sqlc.arg(cart_id),sqlc.arg(sku_id),sqlc.arg(quantity)::bigint)
ON CONFLICT(tenant_id,cart_id,sku_id) DO UPDATE SET quantity=excluded.quantity;

-- name: DeleteCartLine :exec
DELETE FROM app.cart_lines WHERE tenant_id=sqlc.arg(tenant_id) AND cart_id=sqlc.arg(cart_id) AND sku_id=sqlc.arg(sku_id);

-- name: ListCartPriceEntries :many
SELECT l.tenant_id,l.id AS price_list_id,l.currency,l.status,l.priority,
 l.starts_at,l.ends_at,l.audience,l.customer_group_id,l.buyer_company_id,
 e.sku_id,e.min_quantity,e.unit_amount
FROM app.cart_lines cl
JOIN app.price_entries e ON (e.tenant_id,e.sku_id)=(cl.tenant_id,cl.sku_id) AND e.min_quantity<=cl.quantity
JOIN app.price_lists l ON (l.tenant_id,l.id)=(e.tenant_id,e.price_list_id)
WHERE cl.tenant_id=sqlc.arg(tenant_id) AND cl.cart_id=sqlc.arg(cart_id)
 AND l.currency::text=sqlc.arg(currency)::text AND l.status='active' AND l.audience='all'
 AND l.starts_at<=sqlc.arg(at_time)::timestamptz AND (l.ends_at IS NULL OR sqlc.arg(at_time)::timestamptz<l.ends_at)
ORDER BY e.sku_id,l.priority DESC,l.id,e.min_quantity DESC;

-- name: CartAvailability :many
SELECT cl.sku_id,coalesce(sum(s.on_hand-s.reserved),0)::bigint AS available
FROM app.cart_lines cl LEFT JOIN app.stock_levels s ON (s.tenant_id,s.sku_id)=(cl.tenant_id,cl.sku_id)
WHERE cl.tenant_id=sqlc.arg(tenant_id) AND cl.cart_id=sqlc.arg(cart_id)
GROUP BY cl.sku_id ORDER BY cl.sku_id;

-- name: GetActiveCartSKU :one
SELECT v.id FROM app.product_variants v
JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id)
WHERE v.tenant_id=sqlc.arg(tenant_id) AND v.id=sqlc.arg(sku_id)
 AND p.lifecycle_status='ready' AND v.lifecycle_status='ready'
FOR SHARE OF v,p;
