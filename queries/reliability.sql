-- Responsibility: typed invocations of 011's atomic claim and fenced completion.
-- Dependencies: app.idempotency_records/idempotency_claim and transaction-local
-- tenant/claim settings. Caller: platform/idempotency DAO, after authorization.
-- Invariant: no record/result is changed outside the tenant business transaction.

-- name: ClaimIdempotency :one
SELECT r.request_hash::bytea AS request_hash, r.state::text AS state,
 r.claim_token::uuid AS claim_token,
 coalesce(r.response_status,0)::integer AS response_status,
 coalesce(r.response_body_reference,'')::text AS response_body_reference
FROM app.idempotency_claim(sqlc.arg(tenant_id)::uuid,sqlc.arg(actor_kind)::text,
 sqlc.arg(actor_id)::uuid,sqlc.arg(operation)::text,sqlc.arg(idempotency_key)::text,
 sqlc.arg(request_hash)::bytea,sqlc.arg(claim_ttl)::interval,sqlc.arg(claim_token)::uuid) AS r;

-- name: SetIdempotencyClaim :one
SELECT set_config('app.idempotency_claim',sqlc.arg(claim_token)::text,true)::text;

-- name: ReadIdempotencyTenant :one
SELECT coalesce(nullif(current_setting('app.tenant_id',true),''),'')::text AS tenant_id;

-- name: CompleteIdempotency :execrows
UPDATE app.idempotency_records SET state='completed',
 response_status=sqlc.arg(response_status)::integer,
 response_body_reference=sqlc.arg(response_body_reference)::text,
 completed_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND actor_kind=sqlc.arg(actor_kind)::text
 AND actor_id=sqlc.arg(actor_id)::uuid AND operation=sqlc.arg(operation)::text
 AND idempotency_key=sqlc.arg(idempotency_key)::text
 AND request_hash=sqlc.arg(request_hash)::bytea
 AND claim_token=sqlc.arg(claim_token)::uuid AND state='in_progress';

-- name: ListOutboxActiveTenants :many
SELECT tenant_id::text AS tenant_id
FROM app.outbox_active_tenants(sqlc.narg(after_tenant)::uuid,sqlc.arg(tenant_limit)::integer);
