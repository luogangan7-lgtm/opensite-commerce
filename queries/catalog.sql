-- name: GetProduct :one
SELECT id, product_code, lifecycle_status, created_at
FROM app.products
WHERE tenant_id = $1 AND id = $2;

-- name: ListProducts :many
SELECT id, product_code, lifecycle_status, created_at
FROM app.products
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);

-- name: CreateProduct :one
INSERT INTO app.products (tenant_id, product_code, lifecycle_status)
VALUES ($1, $2, 'draft')
RETURNING id, product_code, lifecycle_status, created_at;

-- name: CreateVariant :one
INSERT INTO app.product_variants (tenant_id, product_id, sku, option_key)
VALUES ($1, $2, $3, $4)
RETURNING id, product_id, sku, option_key;

-- name: GetPublishedSite :one
SELECT id, organization_id, published_release_id
FROM app.sites WHERE tenant_id=$1 AND id=$2;
