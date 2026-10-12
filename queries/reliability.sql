-- Responsibility: persist the shared011 fenced command response for quotes.
-- Dependencies: app.idempotency_records; caller:quotes after a successful claim.
-- Invariants: current claim only, same tenant/actor/operation/key, atomic with domain writes.
-- name: QuoteCompleteIdempotency :execrows
WITH fence AS MATERIALIZED (SELECT set_config('app.idempotency_claim',sqlc.arg(claim_token)::uuid::text,true))
UPDATE app.idempotency_records SET state='completed',response_status=sqlc.arg(response_status)::integer,
 response_body=sqlc.arg(response_body)::jsonb,completed_at=clock_timestamp()
FROM fence WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND actor_kind=sqlc.arg(actor_kind)::text
 AND actor_id=sqlc.arg(actor_id)::uuid AND operation=sqlc.arg(operation)::text
 AND idempotency_key=sqlc.arg(idempotency_key)::text AND claim_token=sqlc.arg(claim_token)::uuid AND state='in_progress';

