-- Responsibility: generated order commands and authoritative payable reads.
-- Depends on controlled app orders/inventory functions; used by orders service.
-- Invariants: caller transaction and tenant RLS, no direct snapshot writes.
-- name: GetOrdersTenantContext :one
SELECT nullif(current_setting('app.tenant_id',true),'')::uuid AS tenant_id;

-- name: LockOrderReservations :exec
SELECT app.orders_lock_reservations(sqlc.arg(order_id)::uuid,sqlc.arg(reservation_ids)::uuid[]);

-- name: ReserveOrderInventory :one
SELECT * FROM app.inventory_reserve(sqlc.arg(sku_id)::uuid,sqlc.arg(quantity)::bigint,
 'order',sqlc.arg(order_id)::uuid,sqlc.arg(idempotency_key)::text,sqlc.arg(ttl_seconds)::integer);

-- name: CreateOrder :one
SELECT * FROM app.orders_create(sqlc.arg(id)::uuid,sqlc.arg(creation_key)::text,sqlc.arg(request_hash)::bytea,
 sqlc.narg(customer_id)::uuid,sqlc.narg(guest_contact)::jsonb,sqlc.arg(currency)::text,
 sqlc.arg(shipping_minor)::bigint,sqlc.arg(shipping_snapshot)::jsonb,
 sqlc.arg(lines)::jsonb,sqlc.arg(adjustments)::jsonb);

-- name: GetOrder :one
SELECT * FROM app.orders WHERE tenant_id=$1 AND id=$2 AND sealed;

-- name: GetPayableOrder :one
SELECT id,total_minor,currency,version,state FROM app.orders WHERE tenant_id=$1 AND id=$2 AND sealed;

-- name: TransitionOrder :one
SELECT * FROM app.orders_transition(sqlc.arg(id)::uuid,sqlc.arg(target)::text,
 sqlc.arg(event_key)::text,sqlc.arg(refund_minor)::bigint);

-- Admin reads: sealed snapshots only, newest first by (created_at,id); guest
-- contact never leaves the database, only whether the buyer was a customer.
-- name: ListAdminOrders :many
SELECT id,order_number,state,currency,total_minor,(customer_id IS NOT NULL)::boolean AS is_customer,created_at
FROM app.orders
WHERE tenant_id=sqlc.arg(tenant_id) AND sealed
 AND (sqlc.narg(state)::text IS NULL OR state=sqlc.narg(state)::text)
 AND (NOT sqlc.arg(has_cursor)::boolean OR (created_at,id)<(sqlc.arg(cursor_created_at)::timestamptz,sqlc.arg(cursor_id)::uuid))
ORDER BY created_at DESC,id DESC
LIMIT sqlc.arg(page_size)::integer;

-- name: GetAdminOrder :one
SELECT id,order_number,state,version,currency,tax_minor,shipping_minor,total_minor,
 coalesce(shipping_snapshot->>'Method','')::text AS shipping_method,created_at,updated_at
FROM app.orders WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id) AND sealed;

-- name: ListAdminOrderLines :many
SELECT id,sku,quantity,unit_price_minor,line_tax_minor,line_total_minor
FROM app.order_lines WHERE tenant_id=sqlc.arg(tenant_id) AND order_id=sqlc.arg(order_id) ORDER BY id;

-- name: ListAdminOrderAdjustments :many
SELECT id,reason,amount_minor
FROM app.order_adjustments WHERE tenant_id=sqlc.arg(tenant_id) AND order_id=sqlc.arg(order_id) ORDER BY id;
