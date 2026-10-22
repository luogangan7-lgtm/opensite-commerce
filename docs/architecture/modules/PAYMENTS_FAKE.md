# Fake payment fact ingress (C-19a)

This is an isolated test-provider adapter, not a Stripe integration. No external
provider call or key is used. `OPENSITE_FAKE_PSP=1` requires
`OPENSITE_ENVIRONMENT=test|development`, a runtime database, and a private
`OPENSITE_FAKE_WEBHOOK_KEY` containing 32–128 random bytes encoded as unpadded
base64url. Invalid flags, missing keys, and production mode fail before database
IO. Without the flag, the webhook route does not exist. The E2E harness generates
its own key in its private environment file; do not reuse it as an application key.

## Authority and transaction boundary

The raw body is bounded to 64 KiB and signed as HMAC-SHA256(timestamp + "." + body).
`X-Fake-Signature` is `t=<unix seconds>,v1=<lowercase hex digest>`. Compare the MAC
in constant time. Reject unsigned, altered, ambiguous or unsupported envelopes
before any database work. Both initial and post-lock freshness use the database
clock, within 300 seconds; application wall clocks never authorize a receipt.

Only provisioning can create `payment_provider_accounts`. The account binding is
immutable and unique across tenants. Fake MVP deliberately supports **one account
per tenant**, including disabled accounts; account rotation/multi-account checkout
requires a future explicit persisted intent-to-account binding. This restriction
prevents a second account from claiming an existing checkout's provider reference.
The resolver's NOLOGIN role can only read/lock this directory. Runtime cannot
provision, enable or reassign it. Unknown/disabled accounts log a sanitized refusal.
The wire has no tenant field; the mapped tenant is the only RLS authority.

`payment_events.claimed_order_id` is an untrusted provider assertion, not an order
FK. The table retains composite FKs to the mapped account and existing inbox.
This deliberate exception lets unknown and foreign order claims retain a failed
receipt for reconciliation while tenant-scoped checkout lookup reveals no data.
Only the applier resolves the actual checkout/order under that mapped tenant.
Normalized fields contain no raw payload/contact or provider metadata; inbox stores
only SHA-256 and an internal event reference. Privacy columns are registered.

Lock order is directory row → provider account/intent advisory lock → inbox/event
rows → checkout row → order → reservation IDs → stock → ledger. The inbox identity
is the existing tenant/provider/account/environment/event unique key. Different
bytes under that identity yield 409. All admission, business effects and receipt
updates commit in one transaction. An infrastructure failure rolls them all back.
A savepoint isolates a signed but conflicting business fact: it remains failed
with a fixed `state_conflict` code, and valid peers can still finish. A later
redelivery re-evaluates pending/failed facts; no background worker is required.

## States and money

Capture consumes a completed provider fact; it never issues `Payment.Port.Capture`.
It atomically transitions the order to paid (the existing inventory commit rule),
posts the existing checkout journal and seals the receipt. Failed/canceled facts
cancel unpaid orders and release stock; late failure cannot undo a captured order.
Capture against a competing cancellation is retained for reconciliation, not lost.
An authorization notification is not capture and is rejected without effects.

Refund-before-capture remains pending. Capture is drained before refunds for the
same intent; a refund delta uses `money.Allocate` over remaining net/tax/shipping
components, preserving their total and preventing component over-refund. Cumulative
accepted deltas choose partially_refunded/refunded; provider status alone cannot
force a full refund. Reverse journals conserve debit/credit and clear receivables.
Refunds do not imply returned physical stock. Capture replay and repeated refund
identity create no second journal. Provider calls are forbidden in the consumer.

Existing checkout Start/Confirm/Cancel remain command entrypoints. Fact processing
shares their checkout lock, permanent attempts, claim key and terminal journal
identity. No HTTP handler calls the old tenant-trusting signature verifier; that
compatibility constructor fails closed. The bootstrap composes the fact consumer
only; it does not activate the paused storefront purchase integration.

## Acceptance

CI-only default/pgx tests cover signature/body/time failures, production denial,
account isolation, unknown-order retained failures, concurrent duplicates,
transaction rollback and reordered refunds. SQL gates check RLS, exact privileges,
immutable identities and the account/stream locks. Upgrade gates preserve existing
ready checkout snapshots and initialize refund counters at zero. The MVP harness
sends signed raw webhooks to the real API and independently checks persisted inbox,
order, stock and balanced ledger facts. It seeds ready orders; this is **not** a
browser purchase-flow claim. Actual run IDs and SHAs belong in the PR evidence.
