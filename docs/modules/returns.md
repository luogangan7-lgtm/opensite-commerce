# Returns and refunds

Returns owns `returns`, immutable `return_lines` and durable refund intents.
`createReturn`, `receiveReturn` and `refundReturn` require `returns:write`,
`Idempotency-Key` and a quoted `If-Match` containing the associated **order**
version. Requests contain quantities and provenance; the server computes money.
No client supplies a provider payment reference, ledger proof or refund amount.

`shipment_return` quantities reference the exact shipped order line and cannot
exceed its shipped quantity across returns. Receipt may restore stock once.
`unshipped_cancel` immediately receives and restores only committed, unshipped
units, with the reason and return identity recorded in inventory movements.
Both paths reserve their quantities permanently, including during refund retries.
Pending-payment cancellation remains the existing Orders operation.

## Frozen allocation

The original order snapshot remains immutable. Signed order adjustments are
distributed across line net amounts deterministically (quantity is the fallback
for zero net weights); tax uses each frozen line tax, shipping uses ordered
quantity. Each line's component budget is allocated by cumulative integer floors:
`floor(budget * cumulative_returned / ordered) - floor(budget * prior / ordered)`.
This is monotone, has no negative rounding delta and consumes every remainder
when all units are returned **without earlier direct Orders refunds**.

Legacy direct refunds have no income/tax/shipping attribution. Subtract every
immutable return-line allocation from the original component caps first, then
consume legacy credit from the remaining caps in stable order: order-line UUID,
then income, tax and shipping. Remaining quantities share only those remaining
budgets; their final unit consumes the rounding remainder. Completed returns and
their receipts never change. This also permits a legacy refund between completed
RMAs without making refunded money available again. Active RMAs continue to
exclude new direct Orders refunds, while existing event replay remains available.

Stored amounts use bigint minor units and one currency; intermediate integer
products use exact numeric arithmetic. Invalid negative net budgets fail before
contacting a payment provider. No new tax, promotion, currency conversion or
shipping-price calculation is performed.

Zero-value cancellations enter terminal `completed` after immediate receipt and
once-only stock restoration. Zero-value physical returns remain `requested`
until receipt, then enter `completed` even if another return already fully
refunded the order. Receipt preserves that refunded order state. `completed`
requires zero money, no refund intent and no journal; replay returns the stored
receipt and cannot restore stock twice. These returns do not call the Payment
Port. Positive refunds retain `received` → `refund_pending` → `refunded`.

## Atomic refund budget and lock order

This is the mandatory design contract for plan item 7, recorded before production
implementation. Earlier conservation tests do not establish that this shared
command exists. Implementation and current-source CI evidence are required.
The integration remains migration `034_fulfillment_returns.sql`, after main's
`033_customer_auth.sql`; it must not replace or renumber the customer migration.

Orders owns the single internal command with this frozen SQL identity:

```sql
app.orders_refund_budget(uuid,text,uuid,bigint,text,text,bigint) RETURNS app.orders
```

Its closed modes are `freeze`, `reserve`, `lock`, `complete` and `legacy`.
Its authority is the tenant-scoped sealed order total and confirmed
`refunded_minor`, immutable RMA receipts, and durable `refund_pending` intents.
The command rejects unsupported modes, missing authority, inconsistent receipts,
negative capacity, overflow and conflicting keys rather than accepting caller
money as RMA authority. RMA amounts come from their frozen server receipt;
legacy amounts retain the existing trusted Orders event contract.

Every mode acquires the order row first and performs an actual parent UPDATE
before reading aggregate budgets, then locks all that order's Returns rows in
UUID order with `FOR UPDATE`. This physical write is required for stale
REPEATABLE READ snapshots; a shared row lock alone is insufficient.
The fence writes the parent tuple with `updated_at = updated_at`, preserving
the complete Orders replay snapshot while creating a new MVCC row version.
Confirmed financial changes still set timestamps from `clock_timestamp()`.
Re-read receipt, key and state after these locks. Reads used only to discover an order
ID may precede them, but may not authorize a budget mutation. Creation and
receipt also follow this parent-first discipline, so a new RMA cannot appear
between the locked budget check and its transaction's writes.

Every mode validates the same envelope using exact numeric intermediate sums:
`confirmed + pending <= total`,
`legacy = confirmed - sum(refunded RMA amounts) >= 0`, and
`legacy + sum(ALL sealed RMA amounts) <= total`. Confirmed means the Orders
counter; pending means durable `refund_pending` receipts, not an additional
copy of an already confirmed refund. Invalid authority fails before mutation.

`freeze` validates quantity/quota and this available-money envelope both before
RMA creation and after its sealed receipt is inserted, within the same
transaction. The second call proves the actual allocation still fits the money
budget. It returns the locked Orders snapshot for the existing immutable
component allocator, which retains line UUID / income / tax / shipping
waterfall and integer rounding without independent budget authorization or
receipt rewrites. `reserve` includes the candidate frozen amount only if that
receipt is not already pending; matching pending replay must not double-count
it. Pending receipt writes occur under command locks in the same transaction.
`lock` validates and locks that durable reservation for completion without
consuming it or inventing a new intent.

Only `complete` and `legacy` may increase `orders.refunded_minor`; no wrapper,
batch path or Go caller writes that counter independently. `complete` transfers
the existing RMA reservation to confirmed money exactly once, excluding that
reservation from the remaining pending sum. `legacy` applies the trusted legacy
amount through the same atomic budget check. The confirmed-plus-pending total
must remain within the sealed total before and after either operation. Returns
wrappers update pending/terminal receipts while holding command locks in the
same transaction as the Orders counter, version and event changes. Orders and
Returns cannot split these changes into separately committed operations.

The existing frozen-RMA policy remains: `requested`, `received` and
`refund_pending` RMAs prevent a new legacy direct refund. This additional fence
lives inside the shared command; adopting it does not authorize weakening the
policy. A valid previously recorded legacy event may still replay. Zero-value
RMAs keep the existing once-only restoration and `completed` receipt behavior,
with no provider operation, money counter increment or journal.

The command is owned by `app_orders_executor`, has a fixed search path and
tenant checks, and has no PUBLIC, `app_runtime` or `app_returns_service` direct
EXECUTE grant. Existing Orders wrappers execute as their Orders owner; closed
Returns wrappers invoke it through `app_fulfillment_executor`. Any column
authority is exactly `UPDATE(id) ON app.returns` for the Orders role, solely to
permit `FOR UPDATE`; the existing return-header trigger rejects actual Orders
mutation. The exact role matrix and a negative mutation gate must enforce this
distinction; no other column UPDATE grant, broad table UPDATE or inheritance is
introduced. Matching terminal replay validates the exact durable
key/fingerprint and journal identity using null-safe comparisons. Mismatch or
corrupt authority fails closed; successful replay changes no financial amount,
receipt, version or outbox event. The physical parent fence may use the database
clock; timestamps never come from the caller. An uncertain provider reservation
must not expire or be released merely because time elapsed.

All entrypoints delegate to this command: the legacy Go
`Orders.Transition` / SQL `orders_transition` financial branch, RMA creation,
prepare, completion-lock and complete wrappers, and their HTTP service and retry
callbacks. Nonfinancial Orders transitions retain their existing behavior.
Before Ledger.Post, Go calls `LockPrepared` through the closed
`app.returns_lock_refund(uuid,text)` wrapper, which invokes `lock` mode and
validates the stored return ID/key. Both first completion and recovery use the
same transaction lock order: Order, all Returns by UUID, then Ledger. Provider
Refund stays outside the database transaction after durable preparation. Ledger
implementation remains unchanged; its closed public port owns the journal, and
Returns completion verifies the exact posted receipt before shared-budget
completion. The final HTTP idempotency callback commits or rolls back with the
receipt, journal and Orders change.

Acceptance is CI-only and bound to the changed source. Required negative gates
cover the exact command signature/owner/ACL and a same-OID sentinel replacement
that proves legacy and each RMA phase reach that single command; mixed legacy
and RMA blocking, UUID lock order, stale REPEATABLE READ, same-key replay and
conflicting/null/corrupt authority; and Go `LockPrepared` before Ledger.Post on
first completion and retry. Preserve the existing 150 seeded conservation
cases, 25 terminal/interleaving cases, zero-money and frozen-receipt witnesses,
fresh/populated upgrade tests and mandatory pgx race coverage. Each new guard
needs an injected-fault RED followed by corrected GREEN. This section claims no
test execution or implementation acceptance.

The implementation scope is Orders, Returns and Fulfillment, including their
shared migration and generated integration outputs. New nonblocking findings
must be recorded as `DEFERRED → fulfillment 跟进卡` with a concrete condition
that requires re-upgrade; they do not expand this mandatory contract's scope.

## Durable provider and ledger coordination

A trusted coordinator resolves the original captured payment and ledger accounts.
It verifies tenant, order, currency and the entire original paid total, then
commits a refund intent before calling `PaymentPort.Refund`. The intent freezes
the provider reference, original capture version, PSP key, amount and accounts.
A retry resumes this stored intent before consulting mutable configuration.
Provider-success/database-failure retries reuse the same Payment Port fingerprint.

The final tenant transaction posts a `return` / `refund` ledger journal, debiting
frozen income, tax and shipping amounts and crediting cash by their exact sum.
The completion function verifies the journal's source, accounts and amounts, then
advances the return and cumulative order refund together. Completed replay does
not contact the provider or post another journal.

HTTP commands use the same actor-scoped C-04 protocol described in the Fulfillment
module. Refund completion invokes C-04 inside the final ledger/domain transaction;
response-storage or claim-fencing failure rolls back that transaction. Provider
success remains recoverable through the frozen intent and permanent PSP key.
The response store must persist across restarts and be shared by API instances.
Real actor admission is supplied by C-02b after integration; this module contains
no authentication implementation.

`app_runtime` may request and receive returns but cannot prepare or complete a
refund. The isolated `app_returns_service` capability is reserved for the trusted
coordinator. Database records do not cryptographically prove provider success;
the Payment Port and authenticated coordinator remain the authority. Production
capture/account bindings must be explicitly supplied; missing composition fails
closed, and the production API never creates a Fake provider.

Tests include the actual-PG path: four paid units, two shipped, two canceled and
restocked, one refund despite a finalization failure, and a balanced journal;
returning the remaining shipped units conserves the original total and stock.
Provider recovery, quantity bounds, tenant isolation and illegal transitions have
independent negative tests. No carrier integration, exchange, return label or
real payment-provider adapter is included.
