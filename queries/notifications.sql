-- Responsibility: typed notification worker/admin capability invocations.
-- Depends on 024 notification commands and tenant-local transactions.
-- Caller: notifications PG adapter; invariant: no runtime direct writes.
-- name: ClaimNotificationOutbox :many
SELECT e.* FROM app.claim_notification_outbox(sqlc.arg(tenant_id)::uuid,sqlc.arg(worker_id)::uuid,sqlc.arg(claim_limit)::integer,sqlc.arg(lease_seconds)::integer) AS e;
-- name: BeginNotification :one
SELECT d.* FROM app.notification_begin(sqlc.arg(tenant_id)::uuid,sqlc.arg(dispatch_event_id)::uuid,sqlc.arg(notification_id)::uuid) AS d;
-- name: FinishNotification :one
SELECT app.notification_finish(sqlc.arg(tenant_id)::uuid,sqlc.arg(notification_id)::uuid,sqlc.arg(send_token)::uuid,sqlc.arg(outcome)::text,sqlc.narg(safe_error)::text)::boolean AS finished;
-- name: RedriveNotification :one
SELECT r.* FROM app.notification_redrive(sqlc.arg(tenant_id)::uuid,sqlc.arg(notification_id)::uuid,sqlc.arg(idempotency_key)::text) AS r;
-- name: ListNotificationFailures :many
SELECT * FROM app.notification_deliveries WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND status IN('failed','dead_letter','uncertain') ORDER BY created_at DESC,id DESC LIMIT sqlc.arg(row_limit)::integer;
