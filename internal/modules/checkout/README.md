# Checkout

`contract/` owns transport-neutral commands and consumer ports; `internal/` owns
orchestration and sqlc adapters. Callers authenticate a capability or customer
session before constructing `AuthorizedActor`. Construction validates shape only.
Guest identity is the SHA-256 digest of the authenticated capability; buyer contact
is an immutable order snapshot and grants no authority. Customers come from the
actor, never the JSON body. Quote checkout and zero-value checkout are rejected.

Checkout owns `checkout_sessions` and permanent `checkout_attempts` event receipts.
Both have composite tenant keys/FKs, FORCE RLS, and exact column grants. Orders and
Ledger are invoked only through public contracts. Checkout reads canonical rules
and active configured ledger accounts through sqlc and never creates accounts.

## Starting and pricing

`CartReader` consumes PR31-compatible intent (SKU, quantity, cart version) without
accepting projected prices. Its adapter must check tenant/actor ownership and freeze
the snapshot in the supplied transaction. `NewSQLRules` reads ready catalog SKUs,
active price lists, tax and shipping tables, and database transaction time. Real
pricing, tax and shipping calculators compute checked integer totals. Tax category,
weight and shipping profile currently come from trusted tenant-keyed server
metadata because this catalog base lacks canonical columns; they never come from
request JSON. No authenticated customer group pricing is wired by this card.

Start persists the immutable address, quote, order, stock reservation and original
payable before `CreateIntent`. Provider I/O holds no database transaction or lock.
The first result and original payable version/amount/currency remain immutable.
Permanent `(tenant, actor kind, actor ID, request key)` identity replays the original
result; different request content conflicts before reading or repricing the cart.
This is checkout's permanent identity, not a replacement for the platform lease
mechanism: platform actor kinds cannot truthfully represent a guest capability.

## Confirm, cancellation and recovery

Trusted event tenant/order/version/amount/currency/provider reference are checked
before provider access. Permanent receipts bind event identity to its content.
Confirm and Cancel compete for one persisted phase claim; only the winner may call
the provider. Create, capture and cancel use distinct stable provider keys. The
original claim may resume after a process or database failure using the same key.
The final transaction changes order/inventory, posts the journal and completes the
receipt together. Order transitions emit their existing outbox events in that same
transaction. A repeated event or idempotency key cannot duplicate these effects.

A paid journal records receivable debit against revenue/tax/shipping credits, then
cash debit against receivable credit. Zero components are omitted, configured
account type/currency/status are checked, and Ledger enforces balance. Failed or
cancelled checkout releases inventory through Orders and posts no journal.

Fake `ErrorNonRetryable` means the provider operation had no effect: a permanent
create failure cancels the pending order; a permanent capture failure persists a
`failing` claim, cancels the provider intent, then cancels the order. Retryable or
ambiguous errors preserve the claim. If capture succeeds and the final database
transaction fails, the service preserves `confirming` and repeats the same capture
key; it never claims the provider was rolled back. If a reservation expires before
finalization, compensation/reconciliation needs a later provider card; this card
preserves the claim and returns an error rather than asserting a refund.

## Composition and scope

Production composition has no guest/customer resolver or PR31 cart adapter yet and
therefore denies admission. HTTP binds the target tenant to its trusted actor and
keeps Fake events behind nonproduction configuration and signature verification.
Tests substitute only the cart/resolver boundary and catalog metadata while using
real PostgreSQL rule rows, calculators, Orders, Ledger and the Fake payment port.

This card does not connect Stripe, B2B quote checkout, fulfillment, reporting or
reconciliation. Real money, production deployment and power-loss injection are
NOT_RUN. Source-bound unit, SQL, integration and mutation evidence is recorded in
the PR acceptance receipts; independent review remains the merge boundary.
