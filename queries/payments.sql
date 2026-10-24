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
SELECT d.id,i.provider_event_id,i.payload_hash FROM app.payment_event_deliveries d
JOIN app.webhook_inbox i ON (i.tenant_id,i.id)=(d.tenant_id,d.id)
WHERE d.tenant_id=$1 AND d.payment_event_id=$2 ORDER BY d.id;
-- name: FinishPaymentInbox :exec
SELECT app.finish_payment_inbox(sqlc.arg(id)::uuid,sqlc.arg(success)::boolean);
-- name: GetPaymentReturnContext :one
SELECT app.payments_return_context(sqlc.narg(order_id)::uuid,sqlc.arg(currency)::text)::jsonb AS result;
-- name: LockPaymentReturnCheckout :exec
SELECT app.payments_lock_return_checkout(sqlc.arg(return_id)::uuid);
-- name: BindPaymentReturnRefund :exec
SELECT app.payments_bind_return_refund(sqlc.arg(return_id)::uuid,sqlc.arg(attempt_key)::text,sqlc.arg(provider_refund_id)::text,sqlc.arg(journal_id)::uuid);
-- name: MatchPaymentReturnRefund :one
SELECT app.payments_match_return_refund(sqlc.arg(order_id)::uuid,sqlc.arg(provider_ref)::text,sqlc.arg(provider_refund_id)::text,sqlc.arg(payment_version)::bigint,sqlc.arg(amount_minor)::bigint,sqlc.arg(currency)::text)::text AS result;
-- name: AllocatePaymentRefund :one
SELECT (parts->0->>'income_minor')::bigint AS income_minor,(parts->0->>'tax_minor')::bigint AS tax_minor,
 (parts->0->>'shipping_minor')::bigint AS shipping_minor FROM
 (SELECT app.payments_allocate_refund(sqlc.arg(order_id)::uuid,NULL,sqlc.arg(amount_minor)::bigint) AS parts) allocation;

-- name: FinalizeRefundCompensation :one
SELECT app.payments_finalize_refund(sqlc.arg(fact)::jsonb)::jsonb AS result;
-- name: IsPaymentRefundTerminal :one
SELECT app.payments_refund_terminal(sqlc.arg(account)::text,sqlc.arg(provider_refund_id)::text,sqlc.arg(order_id)::uuid,sqlc.arg(provider_ref)::text,sqlc.arg(payment_version)::bigint,sqlc.arg(amount_minor)::bigint,sqlc.arg(currency)::text)::boolean AS terminal;
-- name: PostPaymentReturnRefund :one
SELECT app.payments_post_return_refund(sqlc.arg(return_id)::uuid,sqlc.arg(attempt_key)::text,sqlc.arg(provider_refund_id)::text)::uuid AS id;

-- Responsibility: Payments' closed reconciliation ports; no cross-module DML.
-- name: RetainRefundCase :one
SELECT app.payments_retain_refund_case(sqlc.arg(fact)::jsonb)::uuid AS id;
-- name: RefundCaseEvidence :one
SELECT app.payments_refund_case_evidence(sqlc.arg(event_id)::text,sqlc.arg(account_id)::text)::jsonb AS result;
-- name: ReplayRefundCase :one
SELECT app.payments_replay_refund_case(sqlc.arg(case_id)::uuid,sqlc.arg(fact)::jsonb)::jsonb AS result;
-- name: ResolveRefundCase :exec
SELECT app.payments_resolve_refund_case(sqlc.arg(case_id)::uuid,sqlc.arg(resolution)::jsonb);
-- name: OpenRefundCases :one
SELECT app.payments_open_refund_cases(sqlc.narg(after_id)::uuid)::jsonb AS result;
