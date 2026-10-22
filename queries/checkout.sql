-- Responsibility: checkout-owned durable sessions/event receipts, authorized
-- read-only calculation rules and configured ledger account IDs. No foreign writes.
-- name: LockCheckoutStart :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 18));
-- name: GetCheckoutByKey :one
SELECT * FROM app.checkout_sessions WHERE tenant_id=$1 AND actor_kind=$2 AND actor_id=$3 AND request_key=$4 FOR UPDATE;
-- name: GetCheckoutByOrder :one
SELECT * FROM app.checkout_sessions WHERE tenant_id=$1 AND order_id=$2 FOR UPDATE;
-- name: GetCheckoutByID :one
SELECT * FROM app.checkout_sessions WHERE tenant_id=$1 AND id=$2 FOR UPDATE;
-- name: InsertCheckout :one
INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,cart_id,cart_version,order_id,currency,amount_minor,items_net_minor,tax_minor,shipping_minor,payable_version,address_snapshot)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING *;
-- name: UpdateCheckout :one
UPDATE app.checkout_sessions SET phase=$3,provider_ref=$4,start_result=$5,final_result=$6,claim_kind=$7,claim_key=$8,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 RETURNING *;
-- name: GetCheckoutAttempt :one
SELECT * FROM app.checkout_attempts WHERE tenant_id=$1 AND event_kind=$2 AND event_key=$3;
-- name: InsertCheckoutAttempt :one
INSERT INTO app.checkout_attempts(tenant_id,event_kind,event_key,checkout_id,request_hash) VALUES($1,$2,$3,$4,$5) RETURNING *;
-- name: CompleteCheckoutAttempt :exec
UPDATE app.checkout_attempts SET result=$4 WHERE tenant_id=$1 AND event_kind=$2 AND event_key=$3;
-- name: GetCheckoutLedgerAccounts :many
SELECT id,code,account_type FROM app.ledger_accounts WHERE tenant_id=$1 AND status='active' AND code IN('cash','accounts_receivable','revenue','tax_payable','shipping_revenue') ORDER BY code;
-- name: GetCheckoutSettings :one
SELECT a.currency,ts.prices_include_tax FROM app.ledger_accounts a JOIN app.tax_settings ts ON ts.tenant_id=a.tenant_id WHERE a.tenant_id=$1 AND a.code='cash' AND a.account_type='asset' AND a.status='active';
-- name: GetCheckoutSKU :one
SELECT v.id,v.sku FROM app.product_variants v JOIN app.products p ON (p.tenant_id,p.id)=(v.tenant_id,v.product_id) WHERE v.tenant_id=$1 AND v.id=$2 AND p.lifecycle_status='ready' AND v.lifecycle_status='ready';
-- name: ListCheckoutTaxLocations :many
SELECT zone_id,country_code,subdivision FROM app.tax_zone_locations WHERE tenant_id=$1 ORDER BY zone_id,country_code,subdivision;
-- name: ListCheckoutTaxCategories :many
SELECT code,is_exempt FROM app.tax_categories WHERE tenant_id=$1 ORDER BY code;
-- name: ListCheckoutTaxRates :many
SELECT id,zone_id,category_code,rate_bp,valid_from,valid_until FROM app.tax_rates WHERE tenant_id=$1 ORDER BY id;
-- name: ListCheckoutShippingLocations :many
-- ADR-C13a: only zones the selected profile has rates for, so another profile's zone cannot shadow them.
SELECT d.zone_id,d.country_code,d.subdivision_code FROM app.shipping_zone_destinations d WHERE d.tenant_id=sqlc.arg(tenant_id)
 AND EXISTS(SELECT FROM app.shipping_rates r WHERE r.tenant_id=d.tenant_id AND r.profile_id=sqlc.arg(profile_id) AND r.zone_id=d.zone_id)
 ORDER BY d.zone_id,d.country_code,d.subdivision_code;
-- name: ListCheckoutShippingRates :many
SELECT * FROM app.shipping_rates WHERE tenant_id=$1 AND profile_id=$2 ORDER BY id;
-- name: GetCheckoutServerTime :one
SELECT transaction_timestamp()::timestamptz AS at_time;

-- Responsibility: checkout's cumulative applied refund components; caller holds
-- its session lock and commits this with Orders/Ledger/payment Inbox completion.
-- name: UpdateCheckoutRefundTotals :one
UPDATE app.checkout_sessions SET refunded_net_minor=sqlc.arg(refunded_net_minor),
 refunded_tax_minor=sqlc.arg(refunded_tax_minor),refunded_shipping_minor=sqlc.arg(refunded_shipping_minor),
 updated_at=clock_timestamp() WHERE tenant_id=sqlc.arg(tenant_id) AND id=sqlc.arg(id) RETURNING *;
