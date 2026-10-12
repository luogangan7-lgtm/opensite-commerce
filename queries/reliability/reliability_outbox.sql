-- Responsibility: worker EXECUTE-only access to 011 lease/fencing functions.
-- Caller: platform/outbox PGStore, each invocation in its own tenant transaction.
-- No direct table privilege, tenant enumeration, dynamic SQL or external I/O.

-- name: ClaimOutboxLease :many
SELECT c.tenant_id::text AS tenant_id,
       c.id::text AS event_id,
       c.delivery_target::text AS delivery_target,
       c.dedup_key::text AS dedup_key,
       c.event_type::text AS event_type,
       c.payload::text AS payload,
       c.lease_owner::text AS worker_id,
       c.attempts::integer AS attempt,
       floor(extract(epoch FROM c.lease_until) * 1000)::bigint AS lease_until_millis
FROM app.claim_outbox(sqlc.arg(tenant_id)::text::uuid,
                     sqlc.arg(worker_id)::text::uuid,
                     sqlc.arg(claim_limit)::integer,
                     sqlc.arg(lease_seconds)::integer) AS c;

-- name: CompleteOutboxLease :one
SELECT app.complete_outbox(sqlc.arg(tenant_id)::text::uuid,
                           sqlc.arg(event_id)::text::uuid,
                           sqlc.arg(worker_id)::text::uuid,
                           sqlc.arg(attempt)::integer)::boolean AS accepted;

-- name: FailOutboxLease :one
SELECT app.fail_outbox(sqlc.arg(tenant_id)::text::uuid,
                       sqlc.arg(event_id)::text::uuid,
                       sqlc.arg(worker_id)::text::uuid,
                       sqlc.arg(attempt)::integer,
                       sqlc.arg(error_code)::text,
                       sqlc.arg(retry_seconds)::integer)::boolean AS accepted;
