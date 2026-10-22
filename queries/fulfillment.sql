-- Responsibilities: generated fulfillment commands and scoped tenant context.
-- Dependencies: controlled fulfillment functions; callers: private store.
-- Invariants: no direct business writes, no second stock commit at shipment.
-- name: GetFulfillmentTenantContext :one
SELECT nullif(current_setting('app.tenant_id',true),'')::uuid AS tenant_id;

-- name: CreateFulfillmentShipment :one
SELECT app.fulfillment_create_shipment(sqlc.arg(order_id)::uuid,
 sqlc.arg(expected_version)::bigint,sqlc.arg(idempotency_key)::text,
 sqlc.arg(carrier)::text,sqlc.arg(tracking_number)::text,sqlc.arg(lines)::jsonb)::jsonb AS result;
