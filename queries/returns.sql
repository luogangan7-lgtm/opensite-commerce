-- Responsibilities: generated return/refund commands and scoped identity read.
-- Dependencies: controlled return functions; callers: private return store.
-- Invariants: prepared refund fields survive provider/database retry boundaries.
-- name: GetReturnsTenantContext :one
SELECT nullif(current_setting('app.tenant_id',true),'')::uuid AS tenant_id;

-- name: GetReturnOrderID :one
SELECT order_id::text FROM app.returns WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND id=sqlc.arg(return_id)::uuid;

-- name: GetReturnsOrderTotal :one
SELECT total_minor,currency FROM app.orders WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND id=sqlc.arg(order_id)::uuid;

-- name: GetPreparedReturnRefund :one
SELECT app.returns_refund_snapshot(sqlc.arg(return_id)::uuid,sqlc.arg(idempotency_key)::text)::jsonb AS result;

-- name: LockReturnRefund :one
SELECT app.returns_lock_refund(sqlc.arg(return_id)::uuid,sqlc.arg(idempotency_key)::text)::jsonb AS result;

-- name: CreateReturn :one
SELECT app.returns_create(sqlc.arg(order_id)::uuid,sqlc.arg(expected_version)::bigint,
 sqlc.arg(idempotency_key)::text,sqlc.arg(kind)::text,sqlc.arg(reason)::text,sqlc.arg(lines)::jsonb)::jsonb AS result;

-- name: ReceiveReturn :one
SELECT app.returns_receive(sqlc.arg(return_id)::uuid,sqlc.arg(expected_version)::bigint,
 sqlc.arg(idempotency_key)::text,sqlc.arg(restock)::boolean)::jsonb AS result;

-- name: PrepareReturnRefund :one
SELECT app.returns_prepare_refund(sqlc.arg(return_id)::uuid,sqlc.arg(expected_version)::bigint,
 sqlc.arg(idempotency_key)::text,sqlc.arg(provider_ref)::text,sqlc.arg(payment_version)::bigint,
 sqlc.arg(income_account_id)::uuid,sqlc.arg(tax_account_id)::uuid,
 sqlc.arg(shipping_account_id)::uuid,sqlc.arg(cash_account_id)::uuid)::jsonb AS result;

-- name: CompleteReturnRefund :one
SELECT app.returns_complete_refund(sqlc.arg(return_id)::uuid,sqlc.arg(idempotency_key)::text,
 sqlc.arg(provider_ref)::text,sqlc.arg(ledger_transaction_id)::uuid)::jsonb AS result;
