-- Responsibility: typed customer authentication commands and credential reads.
-- Dependencies: customer-owned proof functions; caller: dedicated tenant-scoped repository.
-- Invariants: no direct credential DML, opaque hashes and explicit SQL casts.
-- Issuance deadlines use one materialized database-clock reading per command;
-- duration inputs are bounded by the repository and preserve microsecond precision.
-- name: CustomerCredential :one
SELECT c.id AS customer_id,c.email,c.status='active' AS active,(c.verified_at IS NOT NULL)::boolean AS verified,p.password_hash
FROM app.customers c JOIN app.customer_credentials p ON(p.tenant_id,p.customer_id)=(c.tenant_id,c.id)
WHERE c.tenant_id=sqlc.arg(tenant_id)::uuid AND c.email_normalized=sqlc.arg(email)::text;
-- name: PrepareCustomerRegistration :one
WITH issue_clock AS MATERIALIZED (SELECT clock_timestamp() AS issued_at)
SELECT app.customer_prepare_registration(sqlc.arg(email)::text,sqlc.arg(pending_hash)::text,sqlc.arg(token_hash)::bytea,
 issued_at+sqlc.arg(challenge_us)::bigint*interval '1 microsecond')::boolean AS prepared FROM issue_clock;
-- name: ConsumeCustomerVerification :one
SELECT app.customer_consume_verification(sqlc.arg(token_hash)::bytea)::boolean AS consumed;
-- name: PrepareCustomerReset :one
WITH issue_clock AS MATERIALIZED (SELECT clock_timestamp() AS issued_at)
SELECT app.customer_prepare_reset(sqlc.arg(email)::text,sqlc.arg(token_hash)::bytea,
 issued_at+sqlc.arg(challenge_us)::bigint*interval '1 microsecond')::boolean AS prepared FROM issue_clock;
-- name: ConsumeCustomerReset :one
SELECT app.customer_consume_reset(sqlc.arg(token_hash)::bytea,sqlc.arg(new_hash)::text)::boolean AS consumed;
-- name: SaveCustomerSession :one
WITH issue_clock AS MATERIALIZED (SELECT clock_timestamp() AS issued_at)
SELECT coalesce(app.customer_save_session(sqlc.arg(customer_id)::uuid,sqlc.arg(expected_hash)::text,sqlc.narg(old_hash)::bytea,
 sqlc.arg(token_hash)::bytea,issued_at+sqlc.arg(absolute_us)::bigint*interval '1 microsecond',
 issued_at+sqlc.arg(idle_us)::bigint*interval '1 microsecond')::text,'')::text AS document FROM issue_clock;
-- name: GetCustomerSession :one
SELECT coalesce(app.customer_session(sqlc.arg(token_hash)::bytea,sqlc.arg(idle_seconds)::bigint)::text,'')::text AS document;
-- name: RevokeCustomerSession :one
SELECT app.customer_revoke_session(sqlc.arg(token_hash)::bytea)::boolean AS revoked;
-- name: AdmitCustomerAuth :one
SELECT app.customer_admit(sqlc.arg(account_hash)::bytea,sqlc.arg(ip_hash)::bytea,sqlc.arg(now)::timestamptz)::bigint AS cooldown_seconds;
-- name: FailedCustomerAuth :one
SELECT app.customer_failed(sqlc.arg(account_hash)::bytea,sqlc.arg(ip_hash)::bytea)::boolean AS recorded;
-- name: ClaimCustomerGuestOrders :one
SELECT app.customer_claim_guest_orders(sqlc.arg(customer_id)::uuid)::bigint AS linked;
-- name: ListCustomerLinkedOrderIDs :many
SELECT order_id FROM app.customer_guest_order_links WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND customer_id=sqlc.arg(customer_id)::uuid ORDER BY order_id;
