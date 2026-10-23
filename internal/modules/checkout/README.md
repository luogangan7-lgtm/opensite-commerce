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

`NewSQLCart(cart.ReadIntent)` consumes Cart intent (SKU, quantity, cart version
and its original currency) without accepting projected prices. A cart in a different
settlement currency is rejected before order/reservation/provider effects; no FX is performed. The injected Cart contract reader checks
tenant/actor ownership, locks the cart in the supplied transaction, and restores
the original Cart authentication settings before returning. Any read or restoration
failure requires rollback of the borrowed transaction; restoration can fail when
the transaction is already aborted or its context was cancelled. Its supplied transaction
must already carry the matching tenant and effective `app_runtime` role; login
composition sets that role before the callback. The helper never changes its role.
`NewSQLRules` reads ready catalog SKUs,
active price lists, tax and shipping tables, and database transaction time. Real
pricing, tax and shipping calculators compute checked integer totals. Tax category,
weight and shipping profile currently come from trusted tenant-keyed server
metadata because this catalog base lacks canonical columns; they never come from
request JSON. Cart currency must equal the tenant settlement currency before any
order, reservation or provider effect; checkout performs no implicit conversion.
No authenticated customer group pricing is wired by this card.

Missing/malformed acceptance returns `validation_failed` (422).
Start requires explicit `accepted_total.amount_minor` (int64) and `currency`.
It compares both against canonical recomputation inside the Start transaction,
before creating Orders, reservations or provider intents. Mismatch returns
`checkout_total_changed` (409), with no effects; the caller must refresh Preview,
obtain explicit consent and use a new request key. Accepted money is part of the
permanent request fingerprint. Same-key/same-consent retries retain the original
payable even if live prices change; changed consent cannot reuse that key.

The immutable `address_snapshot.accepted_total` is the durable consent receipt,
written with the existing snapshot and protected by the existing guard/grants.
No new table or migration is needed. A new Capture claim (`ready -> confirming`) rechecks this
receipt against the locked immutable amount/currency and payable version.
Terminal paid/attempt replays and already persisted confirming claims do not
require a retroactive receipt; they retain their exact original identity/key. Legacy cart sessions with
no receipt cannot issue a new Capture; uncertain old claims stay preserved for
recovery, never implicitly canceled. Existing sealed-order adoption remains its
separate contract; real-provider adoption is blocked on the C-19b consent
contract in `docs/architecture/V1_DECISIONS.md`. Completed provider facts still record actual money facts,
without invoking Capture or treating a browser assertion as a payment fact.

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
original claim may resume after a database failure using the same key. Process
restart recovery additionally requires the provider to retain that key's result
and intent state. Fake keeps these in memory; its restart durability is NOT_RUN.
The final transaction changes order/inventory, posts the journal and completes the
receipt together. Order transitions emit their existing outbox events in that same
transaction. A repeated event or idempotency key cannot duplicate these effects.

A paid journal records receivable debit against revenue/tax/shipping credits, then
cash debit against receivable credit. Zero components are omitted, configured
account type/currency/status are checked, and Ledger enforces balance. Failed or
cancelled checkout releases inventory through Orders and posts no journal.

Fake `ErrorNonRetryable` means the provider operation had no effect and is an
immutable result for that idempotency key, including concurrent retries. A new
business attempt requires a new key; an ambiguous external result must never be
classified this way. A permanent create failure cancels the pending order; a
permanent capture failure persists a
`failing` claim, cancels the provider intent, then cancels the order. Retryable or
ambiguous errors preserve the claim. If capture succeeds and the final database
transaction fails, the service preserves `confirming` and repeats the same capture
key; it never claims the provider was rolled back. Before Capture, the payment
claim row-locks every order hold through Orders and requires each to be active with
at least 120s left; otherwise it records `failing`, cancels the intent without
capturing and cancels the order. A hold that still expires after that margin (a
capture slower than 120s) needs a later provider card for compensation; the claim
is then preserved and an error returned rather than asserting a refund.

## Composition and scope

Production composition has no guest/customer resolver and therefore denies
admission. The real Cart adapter is available for explicit trusted composition. HTTP binds the target tenant to its trusted actor and
keeps Fake events behind nonproduction configuration and signature verification.
Focused orchestration tests substitute cart intent; integration witnesses also
exercise the real Cart reader in the calling transaction. Authentication resolvers
and catalog metadata are controlled test inputs; PostgreSQL rule rows, calculators,
Orders, Ledger and the Fake payment port are real.

This card does not connect Stripe, B2B quote checkout, fulfillment, reporting or
reconciliation. Real money, production deployment and power-loss injection are
NOT_RUN. Source-bound unit, SQL, integration and mutation evidence is recorded in
the PR acceptance receipts; independent review remains the merge boundary.
