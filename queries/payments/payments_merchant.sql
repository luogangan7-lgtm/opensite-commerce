-- Responsibility: typed dormant Payments owner ports, in the coordinator's tx.
-- name: PaymentsDeletionObligations :one
SELECT app.payments_deletion_obligations(sqlc.arg(tenant_id)::uuid)::jsonb AS result;
-- name: PaymentsEraseMerchantRecords :one
SELECT app.payments_erase_merchant_records(sqlc.arg(tenant_id)::uuid,sqlc.arg(request_id)::uuid,sqlc.arg(epoch)::bigint)::bigint AS count;
