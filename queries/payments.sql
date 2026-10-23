-- Responsibility: payment-owned facts and reliability's inbox ports; callers use
-- the mapped tenant transaction. No raw webhook body or cross-module DML.
-- name: ResolveFakePaymentAccount :one
SELECT tenant_id::uuid,at_time::timestamptz FROM app.resolve_fake_payment_account(sqlc.arg(account)::text);
-- name: ResolveStripePaymentAccount :one
SELECT tenant_id::uuid,at_time::timestamptz FROM app.resolve_stripe_payment_account(sqlc.arg(account)::text);
-- name: LockPaymentStream :one
SELECT app.lock_payment_stream(sqlc.arg(account)::text,sqlc.arg(provider_ref)::text)::timestamptz AS at_time;
-- name: AdmitPaymentInbox :one
SELECT app.admit_payment_inbox(sqlc.arg(account)::text,sqlc.arg(event_id)::text,sqlc.arg(payload_hash)::bytea)::uuid AS id;
-- name: InsertPaymentEvent :exec
INSERT INTO app.payment_events(tenant_id,id,provider_account_id,provider_ref,event_key,claimed_order_id,order_version,status,amount_minor,currency,provider_refund_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING;
-- name: ListPendingPaymentEvents :many
SELECT * FROM app.payment_events WHERE tenant_id=$1 AND provider_account_id=$2 AND provider_ref=$3 AND provider=coalesce(nullif(current_setting('app.payment_provider',true),''),'fake') AND NOT applied
ORDER BY CASE status WHEN 'captured' THEN 0 WHEN 'failed' THEN 2 WHEN 'canceled' THEN 2 ELSE 1 END,created_at,id FOR UPDATE;
-- name: SetPaymentEventOutcome :exec
UPDATE app.payment_events SET applied=$3,error_code=$4 WHERE tenant_id=$1 AND id=$2;
-- name: GetPaymentEvent :one
SELECT * FROM app.payment_events WHERE tenant_id=$1 AND id=$2;
-- name: GetPaymentEventByKey :one
SELECT * FROM app.payment_events WHERE tenant_id=$1 AND event_key=$2;
-- name: HasLegacyPaymentRefund :one
SELECT EXISTS(SELECT 1 FROM app.payment_events WHERE tenant_id=$1 AND provider_account_id=$2 AND provider_ref=$3 AND provider=$4 AND status IN('partially_refunded','refunded') AND provider_refund_id IS NULL);
-- name: BindPaymentDelivery :exec
INSERT INTO app.payment_event_deliveries(tenant_id,id,payment_event_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;
-- name: ListPaymentDeliveries :many
SELECT id FROM app.payment_event_deliveries WHERE tenant_id=$1 AND payment_event_id=$2 ORDER BY id;
-- name: FinishPaymentInbox :exec
SELECT app.finish_payment_inbox(sqlc.arg(id)::uuid,sqlc.arg(success)::boolean);
