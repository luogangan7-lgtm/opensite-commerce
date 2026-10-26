# Payment refund facts

## CI-only live smoke

Authority: `V1_DECISIONS.md`, “Stripe live smoke (CI-only, owner-gated)” (#395).
`NewStripeLiveSmoke` has a separate restricted-live constructor and no production
composition site. It verifies main/dispatch admission and uses API version
`2026-09-30.endive` (official API versioning reference verified 2026-10-26).
The CI-only fixture owns `stripe_smoke.accounts` and `stripe_smoke.inbox`; these
are created only inside the disposable job DB, so no production migration or
test-pinned constraint change is required. Every real financial projection goes
through the existing Payments `applyFact`, Checkout/Orders and Ledger functions.
Fixture SQL is test orchestration, not a new production persistence layer.

The account ID is an additional Environment secret `STRIPE_SMOKE_ACCOUNT`:
Actions prints non-secret step environment values, so a plain variable would
expose a full provider identifier. It scopes every provider request with the
`Stripe-Account` header and maps only this account/run in the ephemeral DB.
The key and email remain exclusively `STRIPE_API` and `STRIPE_SMOKE_EMAIL`.
Invoices and PaymentIntents bind the original run, phase and association before
the private invoice is sent. R locates provider facts and rebuilds a new local
order; no killed-runner file, DB, tenant or local order ID is authoritative.

Only harness create/cancel, Customer/Invoice setup/finalize/send/void, metadata
binding, authenticated Events projection, and Payments refund/recovery are new
write entrances. Production, old batch/async paths and sandbox admission are
unchanged. Timeout uses DB time and is not provider expiry. HTTP never runs
under a DB lock; the live transaction locks stream, Inbox, Checkout/Orders and
Ledger in that order. Refund history is paginated before writes; attempt keys
derive from original run/attempt and durable provider metadata prevents replay
after key retention expires. Pending/unknown and failed/canceled refunds remain
open; automatic attempt increments are forbidden.

Normal CI runs fake-provider lifecycle/output canaries, real PG capture/refund/
reconstruction/replay, and a dispatch/main/Environment/secret-ownership gate.
The owner-only entry is compiled, never executed, by normal PR CI. The secret
step captures and discards all child output and emits only validated receipts;
there is no raw log or artifact upload. Live A/B/R and the separate permission
probe remain NOT_RUN until the owner explicitly starts them after merge.

The endpoint allowlist proves only the application's required capabilities.
Exact manually configured restricted-key grants require independent Dashboard
configuration evidence and the owner-approved probe; the Stripe Apps combined
Charges/Refunds permission is not silently substituted for refund-only access.

References, read 2026-10-26: [API versioning](https://docs.stripe.com/api/versioning),
[invoice line items](https://docs.stripe.com/api/invoice-line-item/bulk),
[restricted keys](https://docs.stripe.com/keys/restricted-api-keys).

Authority: `docs/architecture/V1_DECISIONS.md` §6. Payments owns money refunds;
Returns owns RMA/receipt/restock; Orders owns order state. Payments owns the
single refund journal path and the C-19d terminal-fact finalizer described below.

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
unmatched reconciliation. Confirmed failed/canceled transitions enter the dedicated C-19d finalizer when
that capability is configured. Unconfirmed retrieval and ambiguous legacy
bindings remain retained reconciliation cases. Without the capability, matched
terminal facts retain `refund_terminal_after_match` and metadata-free facts
retain `unmatched_refund`; neither posts a reversal. The generic Ledger primitive
alone cannot authorize financial projection changes.
Terminal failed/canceled API refunds return permanent `NoSideEffect` proof on
both creation and durable recovery; pending/unknown states remain retryable.

Dashboard refunds without `osc_order` are acknowledged as `unmatched`, retained
as failed Stripe Inbox entries with bounded event ID and SHA-256 payload hash,
and emit `payment_webhook_reconciliation_required` / `unmatched_refund`.
No raw provider payload, credential, or customer data is retained. They produce
no normalized order event and no journal. Reconciliation can list failed Stripe
Inbox rows having no associated `payment_events` row, retrieve the immutable
provider event by its event ID, then establish a trusted order/attempt binding.
Part 3 exposes an explicit trusted terminal replay port; no public financial
mutation route is added.

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

Part 3 provides retained cases, explicit recovery/operator ports and observability.
Actual Stripe post-success lifecycle acceptance remains NOT_RUN. An uncertain provider retrieval retains a
failed Inbox without releasing money. No replacement refund is automatic; live
enablement and real post-success Stripe lifecycle acceptance remain separate.

## Reconciliation and recovery (part 3)

`payment_refund_cases`, contributing `payment_refund_case_deliveries` and
`payment_refund_case_resolutions` are tenant-bound, FORCE RLS, append-only.
Known refunds share provider/account/test/refund identity; old unbound Inbox
rows retain account/event identity and their original hash. No amount-based
legacy journal matching is attempted. The public finalizer atomically appends
verified resolution only with the persisted terminal/posted reversal witness.
Case/audit failure rolls back the financial transaction. No process clock or
provider event creation time orders financial state.

Queued refund rejections retain every contributing delivery's own Inbox hash,
even when a different event drains the stream. Recovered legacy case resolutions
append the validated terminal refund identity, so separate unbound deliveries
can reference one existing terminal and reversal without another compensation.

`Stripe.ReplayRefundCase(eventID)` reads retained evidence, commits, retrieves
the immutable event and current refund outside DB locks, and enters the same
Part 2 finalizer through the dedicated ingress pool. Only confirmed terminal
failed/canceled observations may replay; succeeded/nonterminal or conflicting
facts remain open for investigation. Replay preserves the original Inbox hash.
Legacy bindings that the finalizer cannot prove remain quarantined. There is
no background claim/lease worker or automatic PSP mutation in this bounded card.

`ResolveRefundCase` is a trusted composition port, with no public mutation route.
It requires a current owner/admin session or scoped API-key credential, bounded
reason (`external_verified`, `duplicate_confirmed`, `no_posting_required`) and
SHA-256 evidence reference. SQL revalidates authority under locks, preserves the
verifier and DB time, and changes no journal, capacity, RMA or Inbox receipt.
A later verified terminal observation appends its own financial resolution;
the prior operator decision remains visible in history.

Staff GET `/api/v1/t/{tenant_id}/payments/refund-cases` uses the existing
`returns:write` refund scope, authorized read-only snapshot, ID cursor and
100-item bound. It returns only opaque case IDs, bounded reasons and timestamps;
count and oldest age cover the full tenant backlog. `payment_refund_cases_open`
logs those two measurements when read; the existing reconciliation warning
continues on quarantine and `refund_compensation_completed` marks recovery.

Operational procedure: inspect the tenant backlog, investigate immutable event
and authenticated account/capture binding, then explicitly invoke verified
replay or record the reviewed non-posting reason/evidence through the trusted
port. Retrieval errors leave the case open; retry is an explicit supervised
operation. Never send a replacement refund from this procedure. Escalate aging
or contradictory bindings to the Payments owner; production enablement and
provider-balance reconciliation acceptance remain separate owner gates.
