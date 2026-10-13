-- Platform jobs only call controlled inventory service functions; no table writes.
-- name: CurrentJobTenant :one
SELECT coalesce(nullif(current_setting('app.tenant_id',true),''),'')::text AS tenant_id;

-- name: ExpireInventoryJob :one
SELECT app.inventory_expire_due($1::integer)::integer AS expired;
