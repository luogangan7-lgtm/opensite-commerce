# Orders module

Orders owns `orders`, `order_lines` and `order_adjustments`, the sealed
transaction snapshot and lifecycle. It emits `order.created` and `order.<state>`
to the reliability outbox in the same transaction as inventory changes.

`orders.New()` exposes `contract.Service`: `Create`, `Transition` and the
read-only `GetPayable` port. Every method accepts an already authorized
`pgx.Tx`; the caller owns commit and must roll back on every error. The requested
tenant must match `app.tenant_id`. Orders opens no connection or pool, imports
no payment provider, and issues SQL exclusively through generated sqlc queries.

Customer history uses `listCustomerOrders` / `getCustomerOrder` at
`/api/v1/t/{tenant_id}/customer/orders[/{order_id}]`. Bootstrap admits only a
fresh, server-validated customer session; staff/API-key/guest credentials cannot
substitute for it. Every list, detail and line query checks tenant and customer
ownership in SQL inside the existing read-only tenant snapshot. A sealed guest
order is readable only through a persisted `customer_guest_order_links` proof
created by the verified-mailbox claim function; email equality is not proof.
Other customers' orders return 404. The projection contains order number, state,
exact money, creation time and frozen line amounts, without contact/address,
provenance, internal version or staff adjustment fields. Reads make no lifecycle
transition, acquire no new write locks and introduce no application clock.
`internal/db/customer_history_integration_test.go` verifies the production SQL
under runtime RLS, including unverified-email equality, verified linking and
cross-customer denials; bootstrap and desktop/mobile purchase tests verify the
customer cookie boundary and actual registered purchase persistence.

`NewQuoteCreator` exposes the optional `CreateFromQuote` transaction port. It
accepts fixed negotiated money with `QuoteSource` (quote ID, immutable version,
line number), requires one source quote and distinct line numbers, and forbids
price-list/tier provenance and extra adjustments. The source joins the permanent
creation hash; ordinary `Create` rejects it. The trusted caller must verify the
approved quote and bind its durable conversion in this same transaction. Orders
does not authenticate a quote from a constructed value or reprice it.

`NewReservationValidator` exposes `ValidateReservations` before provider capture.
It checks the matching tenant, pending sealed order and exact live holds through
the database-owned function. Provider I/O follows after the transaction ends;
the final paid transition still atomically rechecks and consumes reservations.

Create accepts stable caller-assigned order and line IDs, an idempotency key,
a customer UUID or a frozen guest contact, and previously authorized results
from Pricing, Tax and Shipping. It validates checked Money arithmetic, quantity
binding, tax net plus tax equals gross, inclusive/exclusive price binding, one
currency, and signed adjustments. The JSON snapshots copy the supplied values;
the database seals them and rejects later edits, including owner-role writes.
Customer identity follows the customer contract. The orders migration enforces
`FOREIGN KEY (tenant_id, customer_id) REFERENCES customers(tenant_id, id)` after
the customers migration. A nonexistent or foreign-tenant customer is rejected;
guest orders keep `customer_id` null. This reference proves tenant ownership,
while customer identity/authentication remains the trusted caller's obligation.

Create first serializes its creation key and compares the caller-assigned order ID
and canonical request hash, before locking or reserving inventory. Identical
replays return the stored order even after payment; conflicting reuse consistently
returns `ErrKeyReused`. Tax category snapshots must be 1–64 ASCII lowercase
letters, digits or underscores and valid UTF-8 before sealing.

Create then locks every explicit reservation row (`orders_lock_reservations`,
by id, only holds owned by this order), then automatic reservations acquire SKU
locks in canonical SKU/line order while preserving the input snapshot order.
Reservation rows before stock is the same global order used by transition and
expiry, so mixed explicit/automatic lines for one SKU cannot deadlock a sweep.
Missing reservation IDs request order-owned reservations in the caller's
transaction. Explicit reservation IDs must already belong to this exact order,
SKU and quantity. The database rechecks ownership under lock at creation and
payment/cancellation. Reservation keys are stable hashes of order/line/key;
request hashes precede generated reservation IDs, so creation retries work even
after stock commit. The hash also binds pricing provenance (price list,
priority, minimum quantity). At most 1,000 lines and 1,000 adjustments are
accepted before any stock is reserved. The service limits reservation TTL to 1–86400 seconds and
defaults it to 900 seconds.

Cancellation is allowed only from `pending_payment`. Paid orders require refund
transitions. The Returns module now coordinates paid unshipped cancellation,
controlled restock and refund compensation; see `returns.md`. Shipment continues
to allocate inventory already committed at payment; see `fulfillment.md`.

State boundaries live in `contract.ValidateTransition` and independently in the
database trigger/function. `Transition` validates command shape; the database
checks trusted event deduplication before the current state, so an old payment
event can replay after fulfillment without another inventory commit or outbox
event. Payment events must come from a verified, authorized consumer; this card
exposes no HTTP payment-success endpoint or webhook signature verifier.

`GetPayable` returns the stored total/currency and order version only while
`pending_payment`. Its intent key derives from tenant/order/version. Another
tenant, a missing order or a context mismatch returns `ErrNotFound`. Terminal
or progressed payment state returns `ErrConflict`. SQLSTATE 40001/40P01
(serialization failure, deadlock) map to `ErrRetry`: roll back and retry the
whole transaction; `ErrConflict` is a non-retryable business conflict. The neutral Payment Port
may consume this read-only contract; Orders never imports its implementation.

Tests cover snapshot mismatch and overflow, mutation isolation, state legality,
and stable error classes. Tagged real-PG tests in
`internal/db/orders_integration_test.go` call the public service with production
queries, checking create/replay, authoritative payable, foreign tenant denial,
reservation owner denial, stock/outbox conservation and transaction rollback.
Customer integration tests also seed real customer rows and check the registered
buyer branch, foreign/nonexistent customer rejection, and rollback of any
reservation created by a rejected command.
`tests/db_orders.sql`, `tests/db_orders_guards.sql` and race gates independently
enforce database boundaries. The H-2-C creation guard integration test runs
replay/conflicting reuse in a read-only transaction so attempts to reserve or
lock inventory before key validation fail visibly.

No checkout orchestration, Stripe integration, fulfillment command transport,
customer authentication, exchange or pricing/tax/shipping recalculation is
included. Partial-refund state retains the cumulative amount; the original
payable snapshot remains frozen.

Zero-value orders can expose a zero payable snapshot. The Payment Port refuses
nonpositive intents; future checkout needs a free-order completion path that
bypasses the provider. This card does not implement that orchestration.
