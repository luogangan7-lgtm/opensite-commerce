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
