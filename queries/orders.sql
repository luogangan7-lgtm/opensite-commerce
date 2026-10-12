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
