# Payment refund facts

## CI-only live smoke

Authority: `docs/architecture/V1_DECISIONS.md`, “Stripe live smoke (CI-only,
owner-gated)”, revised by LIVE-A-ONLY (#414). `NewStripeLiveSmoke` is an isolated
permission probe, never production composition or a financial Payment Port.
It requires main/dispatch/first-attempt admission, a restricted live key and the
private expected account. The only permission required is PaymentIntents write.
Only list, create without payment method, and cancel are callable; no money
moves and no financial fact or journal is ingested. Existing run metadata or
uncertainty fails closed without another create; no recovery phase exists.

The key and account remain environment-only `STRIPE_API` and
`STRIPE_SMOKE_ACCOUNT`. Requests pin `2026-09-30.endive`. The disposable DB
provides its clock before HTTP and witnesses zero journals/orders/payment facts;
no live persistence schema or migration is needed. All old and asynchronous
paths and test-mode constraints retain their admission rules.

Normal CI runs fake-only endpoint/admission tests, PG zero-journal witnesses,
workflow/composition static gates and public-output process canaries. The owner
entry compiles separately; raw child output is discarded by the fixed wrapper.
The live workflow remains owner-started and approval-gated; agents never run it.
Payment/refund/reconciliation acceptance stays in test mode and offline fixtures.
Live real-card testing is prohibited by the Stripe SSA; see the contract's dated
[official source](https://docs.stripe.com/testing). Live/production acceptance
remain NOT_RUN.

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
