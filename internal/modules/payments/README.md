# Payment refund facts

Authority: `docs/architecture/V1_DECISIONS.md` §6. Payments owns money refunds;
Returns owns RMA/receipt/restock; Orders owns order state. C-19c unification is
still pending; this PR must not enable the combined Returns/Stripe flow yet.

Stripe `refund.created` and `refund.updated` succeeded deliveries share the
provider refund ID as their financial identity. Each delivery retains its own
exact-body Inbox receipt. Changed amount/order/currency under the same refund
identity is rejected. Pending API refund recovery is retryable with the same
request key; it never proves permanent absence of a provider effect.

Delivery-to-fact bindings persist until the canonical fact reaches a terminal
outcome, including when several refund deliveries arrive before capture.
Legacy facts have no recoverable provider refund ID. Exact-body replays retain
their original journal key; new refund identities on those streams are retained
as failed Inbox deliveries and rejected for trusted reconciliation. No automatic
amount-based matching or rewriting of historical financial identities occurs.

Valid pending and other nonterminal refund facts are ignored without entering
unmatched reconciliation. Matched failed/canceled transitions following success
are retained for reconciliation with reason `refund_terminal_after_match`;
metadata-free terminal refunds retain reason `unmatched_refund`.
They never silently disappear or automatically reverse an existing journal.
**reversal: C-19d** owns the future compensation contract; C-19c-1 preserves the
frozen Orders/Returns transitions and the existing posted-money counters.
Ledger's generic new-event/swapped-side reversal primitive does not define a
provider-refund failure reversal identity, order transition or authorized entry
point; trusted reconciliation must establish those before any corrective posting.
Terminal failed/canceled API refunds return permanent `NoSideEffect` proof on
both creation and durable recovery; pending/unknown states remain retryable.

Dashboard refunds without `osc_order` are acknowledged as `unmatched`, retained
as failed Stripe Inbox entries with bounded event ID and SHA-256 payload hash,
and emit `payment_webhook_reconciliation_required` / `unmatched_refund`.
No raw provider payload, credential, or customer data is retained. They produce
no normalized order event and no journal. Reconciliation can list failed Stripe
Inbox rows having no associated `payment_events` row, retrieve the immutable
provider event by its event ID, then establish a trusted order/attempt binding.
The current implementation does not provide a matching/replay operator command.

**Live-mode prerequisite (NOT_RUN):** an owned reconciliation workflow must
monitor unresolved Inbox facts and provider balances, support authenticated
matching with audit evidence, and prove one journal across request/webhook
replays before live keys or production refunds can be enabled. A successful
sandbox check does not satisfy this live acceptance requirement.

Refund components are allocated by Payments' closed
`app.payments_allocate_refund(uuid,jsonb,bigint)` command for both ordinary
facts and quantity-bounded RMA creation. It takes the Orders physical fence,
locks RMAs by UUID, and subtracts frozen receipts plus Checkout's actual ordinary
component counters. Orders-only legacy credit keeps its prior waterfall.
The existing Checkout component CHECKs remain the final cap guard. Identity-less
refund deliveries retain a failed Inbox/hash without a normalized money fact;
Fake events and results carry the same durable refund identity.

## C-19d preparation (part 1)

The approved `C19D_REFUND_COMPENSATION.md` sequencing starts with identity,
ports and ACLs. `RefundIdentity` prepares a bounded provider/account/refund key
and a swapped copy of persisted Ledger entries. `RefundCompensator` specifies
one whole atomic command, implemented by part 2 below.

`payment_refund_attempts` binds authenticated success facts to a unique posted
original journal. RMA bindings require the sealed Payments success receipt and
matching capture; old unproven identities are not backfilled. `payment_refund_reversals`
requires that original plus a posted, exactly swapped journal with the canonical
transition identity. Both tables are immutable and FORCE RLS. Generic `post_ledger` callers cannot
use the compensation namespace. The isolated `app_payments_service` group is
unprovisioned (NOLOGIN, no members): it owns the dedicated posting command and
reads the proof tables and writes witnesses only inside its closed finalizer. Ledger insertion requires
the canonical refund key, and deferred constraints require exactly one matching
posted journal and witness at commit in either insertion order. No generic runtime/Returns
role can invoke that command or write these tables. These tables are not effective
financial state or a declaration that projections were compensated.

Part 2 provides `payments_finalize_refund(jsonb)` through the dedicated checked
`app_payments_ingress` pool (`OPENSITE_PAYMENTS_DATABASE_URL`). Signed Fake facts
and Stripe current-object-confirmed failures enter that command. It commits the
swapped reversal, witness, terminal identity, Orders/Returns projections, Checkout
component restoration, domain outboxes and processed Inbox together. Closed typed
SQL ports `orders_compensate_refund(uuid)` and `returns_compensate_refund(uuid)`
accept only the posted reversal identity. Each projection has its own witness
trigger; generic transitions and direct SQL cannot decrement confirmed money.
Returns loses generic Ledger posting and requests a Payments-owned sealed RMA
success command. Both success entrypoints honor terminal tombstones. Historic RMA
receipts, stock and allocations remain immutable; compensated entitlements stay
sealed and unavailable to other RMAs. Replacement attempts remain disabled here.

Part 3 adds retained cases/leases, recovery/operator entrypoints, observability
and actual Stripe lifecycle acceptance. An uncertain provider retrieval retains a
failed Inbox without releasing money. No replacement refund is automatic; live
enablement and real post-success Stripe lifecycle acceptance remain separate.
