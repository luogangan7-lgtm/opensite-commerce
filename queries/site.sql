-- name: CreateOrganization :one
INSERT INTO app.organizations(tenant_id, display_name, legal_name)
VALUES ($1, $2, $3)
RETURNING id, display_name, legal_name, created_at;

-- name: GetOrganization :one
SELECT id, display_name, legal_name, created_at
FROM app.organizations
WHERE tenant_id = $1 AND id = $2;

-- name: CreateSite :one
INSERT INTO app.sites(tenant_id, organization_id, name, default_locale)
VALUES ($1, $2, $3, $4)
RETURNING id, organization_id, name, default_locale, created_at;

-- name: GetSite :one
SELECT id, organization_id, name, default_locale, published_release_id, created_at
FROM app.sites
WHERE tenant_id = $1 AND id = $2;

-- name: GetProductVariants :many
SELECT id, sku, option_key, created_at
FROM app.product_variants
WHERE tenant_id = $1 AND product_id = $2
ORDER BY id;
