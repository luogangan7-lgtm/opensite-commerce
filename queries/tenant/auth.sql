-- Responsibilities: typed tenant authorization and API-key custody queries.
-- Dependencies: forward tenant_auth capabilities; private tenant store only.
-- Invariants: no plaintext secrets and no arbitrary credential DML; write-time
-- revalidation runs through the supplied business transaction's Queries.
-- name: TenantMembership :one
SELECT m.user_id,m.tenant_id,m.role FROM app.tenant_memberships m
 JOIN app.users u ON u.id=m.user_id JOIN app.tenants t ON t.id=m.tenant_id
 WHERE m.user_id=sqlc.arg(user_id)::uuid AND m.tenant_id=sqlc.arg(tenant_id)::uuid
 AND m.status='active' AND u.status='active' AND t.status='active';

-- name: TenantAPIKey :one
SELECT k.id,k.tenant_id,k.issued_by,k.key_hash,k.scopes FROM app.api_keys k
 JOIN app.users u ON u.id=k.issued_by JOIN app.tenants t ON t.id=k.tenant_id
 WHERE k.id=sqlc.arg(id)::uuid AND k.revoked_at IS NULL AND u.status='active' AND t.status='active'
 AND (k.expires_at IS NULL OR isfinite(k.expires_at) AND k.expires_at>clock_timestamp());

-- name: TenantListAPIKeys :many
-- Bound custody reads to the newest 100 keys, including revocation metadata.
SELECT k.id,k.key_prefix,k.scopes,k.issued_by,k.created_at,k.expires_at,k.revoked_at,k.last_used_at FROM app.api_keys k
 JOIN app.tenant_memberships m ON m.tenant_id=k.tenant_id AND m.user_id=sqlc.arg(user_id)::uuid
 JOIN app.users u ON u.id=m.user_id JOIN app.tenants t ON t.id=k.tenant_id
 WHERE k.tenant_id=sqlc.arg(tenant_id)::uuid AND m.status='active' AND m.role IN ('owner','admin') AND u.status='active' AND t.status='active'
 ORDER BY k.created_at DESC,k.id LIMIT 100;

-- name: TenantGetAPIKeyView :one
SELECT id,key_prefix,scopes,issued_by,created_at,expires_at,revoked_at,last_used_at FROM app.api_keys
 WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: TenantCreateAPIKey :one
SELECT app.auth_create_api_key(sqlc.arg(tenant_id)::uuid,sqlc.arg(user_id)::uuid,sqlc.arg(id)::uuid,
 sqlc.arg(key_prefix)::text,sqlc.arg(key_hash)::bytea,sqlc.arg(scopes)::text[],sqlc.narg(expires_at)::timestamptz)::uuid AS id;

-- name: TenantRevokeAPIKey :one
SELECT COALESCE(app.auth_revoke_api_key(sqlc.arg(tenant_id)::uuid,sqlc.arg(user_id)::uuid,sqlc.arg(id)::uuid),false)::boolean AS allowed;

-- name: TenantTouchAPIKey :one
SELECT COALESCE(app.auth_touch_api_key(sqlc.arg(id)::uuid),false)::boolean AS allowed;

-- name: TenantRevalidateAuthorization :one
SELECT COALESCE(app.auth_revalidate_tenant(sqlc.arg(tenant_id)::uuid,sqlc.arg(user_id)::uuid,
 sqlc.arg(kind)::text,sqlc.arg(credential_id)::uuid,sqlc.arg(scope)::text),false)::boolean AS allowed;

-- name: TenantSetContext :exec
SELECT set_config('app.tenant_id',sqlc.arg(tenant_id)::uuid::text,true);
