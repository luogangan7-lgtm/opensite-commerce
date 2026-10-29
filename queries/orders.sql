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

-- name: CheckOrderCreation :many
SELECT * FROM app.orders_check_creation(sqlc.arg(id)::uuid,sqlc.arg(creation_key)::text,sqlc.arg(request_hash)::bytea);

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

-- The admin read path refuses to run in a writable transaction (fail closed).
-- name: GetOrdersTransactionReadOnly :one
SELECT current_setting('transaction_read_only')::boolean AS read_only;
-- name: ListOrderReservationIDs :many
SELECT reservation_id FROM app.order_lines WHERE tenant_id=$1 AND order_id=$2 ORDER BY reservation_id;

-- name: CountLiveOrderReservations :one
-- Read after LockOrderReservations; locked rows reflect the latest committed state.
SELECT count(*) FILTER(WHERE state='active' AND expires_at>clock_timestamp()+make_interval(secs=>sqlc.arg(min_remaining_seconds)::integer))::bigint AS live,
 count(*)::bigint AS total
FROM app.inventory_reservations WHERE tenant_id=sqlc.arg(tenant_id) AND id=ANY(sqlc.arg(reservation_ids)::uuid[]);

-- Customer history: the authenticated actor is a SQL predicate, never a Go filter.
-- name: ListCustomerOrders :many
SELECT o.id,o.order_number,o.state,o.currency,o.total_minor,o.created_at
FROM app.orders o
WHERE o.tenant_id=sqlc.arg(tenant_id)
 AND (o.customer_id=sqlc.arg(customer_id) OR (o.customer_id IS NULL AND EXISTS(
  SELECT 1 FROM app.customer_guest_order_links k WHERE k.tenant_id=o.tenant_id AND k.order_id=o.id AND k.customer_id=sqlc.arg(customer_id))))
 AND o.sealed
 AND (NOT sqlc.arg(has_cursor)::boolean OR (o.created_at,o.id)<(sqlc.arg(cursor_created_at)::timestamptz,sqlc.arg(cursor_id)::uuid))
ORDER BY o.created_at DESC,o.id DESC LIMIT sqlc.arg(page_size)::integer;

-- name: GetCustomerOrder :one
SELECT o.id,o.order_number,o.state,o.currency,o.total_minor,o.created_at,o.shipping_minor,o.tax_minor
FROM app.orders o
WHERE o.tenant_id=sqlc.arg(tenant_id) AND o.id=sqlc.arg(id) AND o.sealed
 AND (o.customer_id=sqlc.arg(customer_id) OR (o.customer_id IS NULL AND EXISTS(
  SELECT 1 FROM app.customer_guest_order_links k WHERE k.tenant_id=o.tenant_id AND k.order_id=o.id AND k.customer_id=sqlc.arg(customer_id))));

-- name: ListCustomerOrderLines :many
SELECT l.id,l.sku,l.quantity,l.unit_price_minor,l.line_tax_minor,l.line_total_minor
FROM app.order_lines l JOIN app.orders o ON o.tenant_id=l.tenant_id AND o.id=l.order_id
WHERE o.tenant_id=sqlc.arg(tenant_id) AND o.id=sqlc.arg(order_id) AND o.sealed
 AND (o.customer_id=sqlc.arg(customer_id) OR (o.customer_id IS NULL AND EXISTS(
  SELECT 1 FROM app.customer_guest_order_links k WHERE k.tenant_id=o.tenant_id AND k.order_id=o.id AND k.customer_id=sqlc.arg(customer_id))))
ORDER BY l.id;

-- name: GetOrdersMerchantObligations :one
SELECT count,object_ids FROM app.orders_merchant_obligations(sqlc.arg(tenant_id)::uuid);

-- name: EraseOrdersMerchant :one
SELECT app.orders_erase_merchant(sqlc.arg(tenant_id)::uuid,sqlc.arg(request_id)::uuid,sqlc.arg(epoch)::bigint)::bigint AS erased;
