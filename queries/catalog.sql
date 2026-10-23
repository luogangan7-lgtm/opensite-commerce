-- name: GetProduct :one
SELECT * FROM app.products WHERE tenant_id = $1 AND id = $2;

-- name: ListProducts :many
SELECT * FROM app.products
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (NOT sqlc.arg(has_cursor)::boolean OR (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(page_size);

-- name: CreateProduct :one
-- Match CSV's tenant/canonical-code lock before entering the unique index.
WITH code_lock AS MATERIALIZED (
 SELECT pg_advisory_xact_lock(hashtextextended('catalog-csv:' || sqlc.arg(tenant_id)::uuid::text || ':p:' || sqlc.arg(product_code)::text, 0))
)
INSERT INTO app.products (tenant_id, product_code, lifecycle_status)
SELECT sqlc.arg(tenant_id)::uuid, sqlc.arg(product_code)::text, 'draft' FROM code_lock RETURNING *;

-- name: UpdateProduct :one
UPDATE app.products SET product_code = COALESCE(sqlc.narg(product_code)::text, product_code),
 lifecycle_status = COALESCE(sqlc.narg(lifecycle_status)::text, lifecycle_status), version = version + 1
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id)
 AND version = sqlc.arg(version) AND lifecycle_status <> 'archived'
RETURNING *;

-- name: ArchiveProduct :one
UPDATE app.products SET lifecycle_status = 'archived', version = version + 1
WHERE tenant_id = $1 AND id = $2 AND version = $3 AND lifecycle_status <> 'archived'
RETURNING *;

-- name: CreateVariant :one
INSERT INTO app.product_variants (tenant_id, product_id, sku, option_key)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetVariant :one
SELECT * FROM app.product_variants WHERE tenant_id = $1 AND product_id = $2 AND id = $3;

-- name: ListVariants :many
SELECT * FROM app.product_variants
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id)
  AND (NOT sqlc.arg(has_cursor)::boolean OR (created_at, id) > (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY created_at, id LIMIT sqlc.arg(page_size);

-- name: UpdateVariant :one
UPDATE app.product_variants SET sku = COALESCE(sqlc.narg(sku)::text, sku),
 lifecycle_status = COALESCE(sqlc.narg(lifecycle_status)::text, lifecycle_status), version = version + 1
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id)
 AND id = sqlc.arg(id) AND version = sqlc.arg(version) AND lifecycle_status <> 'archived'
RETURNING *;

-- name: ArchiveVariant :one
UPDATE app.product_variants SET lifecycle_status = 'archived', version = version + 1
WHERE tenant_id = $1 AND product_id = $2 AND id = $3 AND version = $4 AND lifecycle_status <> 'archived'
RETURNING *;

-- name: GetCatalogOptionValue :one
SELECT value.option_id, value.id FROM app.product_option_values AS value
WHERE value.tenant_id = $1 AND value.product_id = $2 AND value.id = $3
 AND NOT EXISTS (
  SELECT 1 FROM app.product_option_values AS other
  WHERE other.tenant_id = value.tenant_id AND other.product_id = value.product_id
   AND other.id = value.id AND other.option_id <> value.option_id
 );

-- name: AddVariantOptionValue :exec
INSERT INTO app.variant_option_values (tenant_id, product_id, variant_id, option_id, value_id)
VALUES ($1, $2, $3, $4, $5);

-- name: GetCatalogProductForUpdate :one
SELECT * FROM app.products WHERE tenant_id = $1 AND id = $2 FOR UPDATE;

-- name: ListCatalogVariantOptionValues :many
SELECT variant_id, value_id FROM app.variant_option_values
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id)
  AND variant_id = ANY(sqlc.arg(variant_ids)::uuid[])
ORDER BY variant_id, option_id;

-- name: CountCatalogProductOptions :one
SELECT count(*) FROM app.product_options WHERE tenant_id = $1 AND product_id = $2;

-- name: GetProductVariants :many
SELECT id, sku, option_key, created_at
FROM app.product_variants
WHERE tenant_id = $1 AND product_id = $2
ORDER BY id;
