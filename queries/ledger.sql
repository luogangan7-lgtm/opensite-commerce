-- Responsibility: atomic posting and journal/account reads for ledger store.
-- Dependencies: app.post_ledger and tenant-scoped ledger tables; caller: ledger.
-- Invariants: authenticated tenant transaction; positive integer money; sums fail
-- on bigint overflow; missing accounts yield no row; cutoff includes posted time.
-- name: PostLedger :one
SELECT app.post_ledger(sqlc.arg(tenant_id)::uuid,sqlc.arg(currency)::text,
 sqlc.arg(source_type)::text,sqlc.arg(source_id)::text,sqlc.arg(event)::text,
 sqlc.arg(accounts)::uuid[],sqlc.arg(sides)::text[],sqlc.arg(amounts)::bigint[])::uuid AS ledger_transaction_id;

-- name: GetLedgerTransaction :one
SELECT tenant_id,id,currency,source_type,source_id,event,status,created_at,posted_at
FROM app.ledger_transactions WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND id=sqlc.arg(transaction_id)::uuid AND status='posted';

-- name: LedgerAccountBalance :one
SELECT a.id AS account_id,a.account_type,a.currency,
 coalesce(sum(e.amount) FILTER(WHERE e.side='debit' AND t.id IS NOT NULL),0)::bigint AS debits,
 coalesce(sum(e.amount) FILTER(WHERE e.side='credit' AND t.id IS NOT NULL),0)::bigint AS credits
FROM app.ledger_accounts a
LEFT JOIN app.ledger_entries e ON e.tenant_id=a.tenant_id AND e.account_id=a.id
LEFT JOIN app.ledger_transactions t ON t.tenant_id=e.tenant_id AND t.id=e.transaction_id
 AND t.status='posted' AND t.posted_at<=sqlc.arg(cutoff)::timestamptz
WHERE a.tenant_id=sqlc.arg(tenant_id)::uuid AND a.id=sqlc.arg(account_id)::uuid
GROUP BY a.tenant_id,a.id,a.account_type,a.currency;
