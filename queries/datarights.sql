-- Data rights query boundary; controlled functions only, no direct PII writes.
-- Depends on 024; generated db callers use authorized tenant pgx.Tx.
-- name: DataRightsTenantContext :one
SELECT nullif(current_setting('app.tenant_id',true),'')::uuid AS tenant_id;
-- name: DataRightsRequestExport :one
SELECT app.data_rights_request_export(sqlc.arg(id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid)::jsonb AS payload;
-- name: DataRightsStatus :one
SELECT app.data_rights_status(sqlc.arg(id)::uuid,sqlc.arg(customer_id)::uuid)::jsonb AS payload;
-- name: DataRightsBuildExport :one
SELECT app.data_rights_build_export(sqlc.arg(id)::uuid)::jsonb AS payload;
-- name: DataRightsCompleteExport :one
SELECT app.data_rights_complete_export(sqlc.arg(id)::uuid,sqlc.arg(token_hash)::bytea,sqlc.arg(content_hash)::bytea,sqlc.arg(download_expires_at)::timestamptz)::jsonb AS payload;
-- name: DataRightsDownloadInfo :one
SELECT app.data_rights_download_info(sqlc.arg(id)::uuid,sqlc.arg(customer_id)::uuid)::jsonb AS payload;
-- name: DataRightsConsumeExport :one
SELECT app.data_rights_consume_export(sqlc.arg(id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(token_hash)::bytea)::jsonb AS payload;
-- name: DataRightsEraseCustomer :one
SELECT app.data_rights_erase_customer(sqlc.arg(customer_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid)::jsonb AS payload;

-- name: DataRightsDownloadDeadline :one
SELECT app.data_rights_download_deadline(sqlc.arg(id)::uuid)::timestamptz AS deadline;

-- name: DataRightsWorkerInfo :one
SELECT app.data_rights_worker_info(sqlc.arg(id)::uuid)::jsonb AS payload;

-- name: DataRightsArtifactLive :one
SELECT app.data_rights_artifact_live(sqlc.arg(id)::uuid)::boolean AS live;
