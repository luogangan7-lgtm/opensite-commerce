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
