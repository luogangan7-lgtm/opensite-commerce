# Ledger contract

The ledger module owns `ledger_accounts`, `ledger_transactions` and
`ledger_entries`. Its public command uses a caller-authorized `pgx.Tx`; it never
opens, commits, rolls back, sets tenant authority, or authenticates a transaction.
The caller owns rollback on any error, commit after success, and transaction-level
retry. No HTTP endpoint or pool belongs to this module.

`Post` accepts 2–1000 ordered positive entries in one uppercase three-letter
currency, with exact balanced debit/credit totals. The permanent identity is
`(tenant_id, source_type, source_id, event)`: identical ordered content returns the
original transaction ID; changed currency, account, side, amount or order returns
`ErrConflict`. A tenant without a configured settlement currency is rejected by
the database. Every database invariant is checked independently of Go validation.

`Balance` sums entries whose transaction `posted_at <= cutoff`, returns currency,
debits, credits and normal net. Asset/expense net is debit minus credit;
liability/equity/revenue net is credit minus debit. An unknown or invisible account
returns `ErrNotFound`, and checked aggregate overflow returns `ErrOverflow`.
Reversal uses a new event and swapped sides; the original rows remain immutable.

The public port is `Module.Post`/`Module.Balance`, with values and stable errors in
`contract/`. SQLC queries live in the owning ledger query file; implementation
lives in the private service/store packages. No events are emitted or subscribed.
`ledger_final_balance` must retain its default deferred mode while `Post` runs.
Callers may force constraints immediately after posting, and must commit or
roll back their own transaction. A failed database statement requires rollback
to a caller-owned savepoint or rollback of the transaction before retrying.
