# C-19d — Refund compensation contract

**PROPOSAL — pending orchestrator approval.** Refs [#310](https://github.com/luogangan7-lgtm/opensite-commerce/issues/310).
Read on 2026-10-23 against main `9e46ea781ad2e957b38bf234dfee876f70cf59ed`.
This docs-only card changes no executable contract, migration, grant or live gate.
Approval must precede implementation cards; existing refusal/reconciliation behavior stays in force.

## Authority and boundary

- [V1 decisions](V1_DECISIONS.md) §§3–4, 6 and overriding §9: Payments owns PSP
  refunds, Ledger owns immutable accounting, Orders owns state, Returns owns RMA.
- [Application decisions](APP_LAYER_DECISIONS.md) A1–A7: public ports, static
  composition, tenant authority, transactional idempotency and reliable events.
- [Runtime capability](../../deploy/runtime-roles.md#fulfillment-and-refund-coordinator),
  [Orders](../modules/orders.md), [Returns](../modules/returns.md),
  [Payments](../../internal/modules/payments/README.md),
  [Ledger](../../internal/modules/ledger/README.md) and their `contract/` sources.
- [#292](https://github.com/luogangan7-lgtm/opensite-commerce/pull/292), fixed source
  `cecfe8a41c82b2c5eb18e8d5c12088cb7e96aefa`: post-success failed/canceled facts
  retain bounded event ID/hash in failed Inbox and warn; they do not reverse money.
  The baseline Payments README states this same limitation. No dedicated resolved
  compensation-case lifecycle or operator matching command is implemented there.

Stripe official references (current online docs, read **2026-10-23**):

- [Refund lifecycle](https://docs.stripe.com/refunds#failed-refunds): bank rejection
  can return funds; cancellation is a failure variant. Some methods can return
  from succeeded to requires_action. This proposal handles final failure/cancel
  after local posting; it does not assume every method follows that sequence.
- [Refund object](https://docs.stripe.com/api/refunds/object): immutable refund ID,
  amount/currency/charge binding and failure_balance_transaction for the adjustment.
- [Event types](https://docs.stripe.com/api/events/types#refund.updated): consume
  refund.created, refund.updated and refund.failed through the same classifier.
- [Delivery rules](https://docs.stripe.com/webhooks#event-ordering): deliveries can
  repeat or arrive out of order; event timestamps are not a sequencing authority.

## Data authority and states

Payments owns a durable refund-attempt fact keyed by
`(tenant, provider, provider_account, provider_refund_id)` (R). It binds one order,
capture, currency, positive minor amount, optional RMA/attempt and original journal.
Bindings are established from authenticated account mapping and frozen intent;
provider metadata alone is never tenant/order authority. Identity changes conflict.
Ledger's posted entries are the authority for the reversal's accounts and amounts;
neither current account configuration nor a new allocation calculation is used.
Legacy journals without a proven unique R binding require manual reconciliation.

Local attempt states below are proposed Payments states, distinct from PSP statuses:

| Current | Verified observation | Next and committed effects |
|---|---|---|
| absent/prepared/pending | pending/requires_action/unknown | pending or reconcile; hold reservation; no posting |
| prepared/pending/reconcile | succeeded | posted; one success journal, confirm Orders money, RMA refunded |
| prepared/pending/reconcile, never posted | terminal failed/canceled | failed_unposted; no journal; release only with permanent exact-key no-effect proof |
| posted | pending/requires_action/ambiguous | reconcile_posted; retain original money and capacity; investigate |
| posted/reconcile_posted | confirmed final failed/canceled | compensated; one reverse journal and atomic projections below |
| compensated | duplicate final failure/cancel or stale success/pending | compensated; receipt only, no financial change |
| compensated | contradictory fresh provider success | reconciliation conflict; no automatic re-post or reactivation |

Persist provider observations separately from effective local financial state.
No missing success delivery causes a fictitious success/reversal pair: if a final
failure arrives before posting, verify the current provider object and freeze a
terminal tombstone for R. Later stale success cannot post. An uncertain retrieval
holds the intent and case open. Only the closed Payments finalizer can resolve it.
`NoSideEffect` means no prior effect: **never** apply it to an already posted R.

## Exactly-once compensation transaction

Only Payments requests Ledger.Post for either refund success or compensation.
Returns requests money through a public Payments port; Orders and Returns do not
post refund journals, call Stripe for compensation, or write Payments tables.
Ledger still validates and owns all Journal/Entry writes through its public port.
No cross-module private store import or Orders → Payments dependency is allowed.

The canonical transition is `posted_to_compensated/v1`, shared by failed AND
canceled observations. Unique `(R, transition)` admits at most one compensation;
raw event ID, webhook type, delivery count and failure reason are not money keys.
Ledger identity is `(tenant, source_type=payment_refund, source_id=encode(provider,
account,refund_id), event=posted_to_compensated/v1)` with an unambiguous bounded
encoding. Identical replay returns the original reversal ID; changed content fails.
Persist a unique original-journal → reversal link as a second fence so aliases or
legacy mapping errors cannot reverse one success twice. Reject ambiguous binding.

Before posting, verify the trusted current refund is failed/canceled and its
capture/amount/currency equal R; freeze the authenticated observation and failure
balance reference when supplied. Missing/contradictory evidence opens a case,
not capacity. Provider retrieval occurs outside DB transactions; recheck the
attempt revision under lock and discard/retrieve again if another writer won.

In one authorized tenant transaction, Payments:

1. Locks account mapping → payment stream → raw Inbox → canonical attempt/fact
   → Order → all its Returns in UUID order → original/new Ledger journals.
   All success, compensation and recovery paths use this same order. Returns
   prepares under Order → Returns locks, commits, then calls Payments; it must
   never call back into Payments while holding those locks. No PSP I/O under locks.
2. Rechecks unique transition, original posted receipt and current effective state;
   acquires the Orders-owned shared budget fence without changing confirmed money.
   It performs the existing real parent UPDATE before aggregate reads, including
   under REPEATABLE READ. Locking alone cannot authorize a stale budget snapshot.
3. Posts a new Journal with identical ordered entries/amounts/currency and swapped
   debit/credit sides. Never edits/deletes the old Journal or its source identity.
4. Through Orders' new `compensate` mode and Returns' public closed command,
   validates the now-posted reversal and subtracts this exact successful
   attempt once from confirmed money; appends compensation receipts and events;
   updates RMA financial status and attempt state. No inventory or quantity write.
5. Marks the case resolved_compensated and Inbox processed; commits the journal,
   projections, idempotency witness and outbox together. Any failure rolls all back.

40001/40P01 retries roll back the whole transaction with the same transition key.
Crash after commit replays that receipt; crash before commit leaves the durable
case retryable. Attempt history and old HTTP success receipts remain immutable;
replay returns the historic response, while fresh reads expose compensated status.

## Restored amount, Orders and RMA effects

Define `C = successful posted amounts - compensated amounts`, `P = unresolved
unposted reservations`, `T = sealed captured refundable total`, all in one currency.
`orders.refunded_minor` becomes **net confirmed C**, not a gross lifetime counter.
Require `0 <= C`, `0 <= P`, `C + P <= T` with exact intermediate arithmetic.
One compensation of A changes C to C-A and restores A of financial capacity.
It does not refund the customer again or erase the merchant's refund obligation.
Gross successes/reversals remain separately auditable from immutable journals.
Provider fees/disputes have separate accounting; this reverses refund principal,
not an assumption that Stripe returns original processing fees.

For an RMA, preserve its sealed component entitlement and all permanently consumed
quantities; mark its money projection `refund_compensated`. It holds no in-flight
reservation, but remains an active RMA for the legacy-direct-refund exclusion.
It may request a replacement attempt only with a **new** key and current version;
then `refund_compensated → refund_pending → refunded` on the same entitlement.
Each attempt has its own provider ID, journal and optional reversal; old attempt
keys remain terminal. Never overwrite the old provider/journal receipt to retry.
Other RMAs cannot appropriate its sealed allocation. Zero-money completed RMAs
remain completed with no provider operation, journal or compensation transition.

Recompute the Orders refund projection under the same budget lock: C=T yields
refunded; 0<C<T yields partially_refunded; C=0 reveals paid/partially_fulfilled/
fulfilled from authoritative committed shipment quantities. Refund must not erase
that fulfillment basis; backfill/derive it from durable fulfillment facts before
enabling this transition, or refuse with reconciliation. Never reopen cancelled
or pending_payment, release payment stock, undo receipt/restock, or ship twice.
Increment Orders version once per new compensation; emit one compensation event
including original/reversal IDs. Existing order snapshots/prices/taxes stay frozen.

## Frozen clauses requiring explicit approval and forward revision

These are proposed exceptions, not permission to weaken current guards. Implement
in new pending stable-ID migrations; never edit applied/frozen migrations.

| Frozen source and clause | Proposed revision |
|---|---|
| Orders `contract/state.go` and `docs/modules/orders.md`: refunded terminal, partially_refunded only self/refunded | Keep generic Transition closed; add a typed, Payments-authorized compensation port for the projection edges above. Independent SQL guard checks exact evidence, not caller target state. |
| `c20a-fulfillment_fulfillment_returns.sql` order guard: refunded_minor never decreases, refund-state changes require increases | Allow only Orders-owned compensate command with unique posted reversal witness to decrement C and bump version; all direct SQL and generic legacy commands still reject it. |
| Returns “Atomic refund budget”: closed modes freeze/reserve/lock/fail/complete/legacy; only complete/legacy increase confirmed | Add closed compensate mode; it alone subtracts exact linked success. Every old/new entrypoint retains the common parent fence. |
| Same section: legacy = confirmed - sum(refunded RMA), ALL sealed RMA allocation envelope | Use net effective successful RMA attempts; compensated entitlement stays sealed once, never counted as both pending and confirmed. Derive net legacy from linked successful direct attempts minus reversals. Preserve both envelopes and legacy exclusion. |
| Returns “Frozen allocation”: completed returns/receipts never change; positive terminal refunded | Preserve line allocation/receipt/restock; introduce refund_compensated financial projection and append-only attempt receipts. Extend status checks/header guard and public response enum, not receipt contents. |
| Returns “Provider rejection”: failure cannot regress completed; only no-effect permits refund_failed/new key | Keep refund_failed strictly never-posted. Introduce proven post-success compensation and new-key replacement via the distinct state, not a weakened fail command. |
| Returns “Durable provider and ledger coordination”, Config.Ledger/LedgerPoster and final return/refund journal | Move all refund posting authority to Payments; Returns uses request/receipt ports. Preserve existing return/refund journal IDs and validated R bindings; never re-key history. Coordinate with C-19c-1, not a second simultaneous success writer. |
| Runtime roles: app_returns_service prepares/completes and has ledger-post capability | Remove Returns refund-post capability when Payments finalizer lands. Add only a dedicated authenticated Payments coordinator capability with closed ports, tenant FORCE RLS and exact ACL matrix; no runtime/renderer grants. |
| Payments README/#292: non-succeeded after success produces failed Inbox only | Keep raw evidence; add the above verified compensation finalizer and retained case resolution. Ambiguous/unbound facts still produce no journal. |
| Ledger Post identity and immutable posted entries | Unchanged; new transition key and reverse link specialize the existing swapped-side primitive. V1 §6 module ownership and A1 dependency direction remain unchanged. |

Update Go ports, DB guards, registry dependencies, OpenAPI response enums and exact
role/scope gates together where affected; no new public financial mutation scope
or route is proposed. Existing authenticated refund admission remains mandatory.

## Reconciliation lifecycle, ingestion and observability

Payments owns a retained case keyed by R+transition, or by authenticated account+
event ID when R is unbound. Lifecycle: open → investigating → ready →
resolved_compensated / resolved_no_posting; transient errors return ready to open;
contradictions remain investigating. Claims have DB-clock lease/retry timestamps,
bounded backoff and a supervised worker wakeup. Expired work claims never release
money reservations. Resolution retains reason, verifier, evidence hash/reference,
original/reversal IDs and DB timestamps; no deletion or rewriting audit history.
Link each contributing Inbox event to the case; keep its exact hash/body-conflict
check. Distinct event IDs for one R do not multiply cases or financial effects.

All writers converge on the same finalizer: synchronous provider result/recovery,
refund.created/updated/failed, out-of-order replay, Dashboard/bulk observations,
worker retry and authenticated operator matching. Operator matching supplies a
reviewed binding, never direct journal edits or arbitrary amounts. Old legacy
Orders paths must not create an unbound parallel PSP success writer. Unknown
metadata/account/mode, conflicting binding or unsigned input fails closed.
Check livemode/test account mapping; isolate accounts even if IDs collide.

DB clock supplies posted_at, resolved_at, observation/lease times and retry due;
provider created is evidence only. Never use process time, last-arrival-wins or
event timestamp comparison to choose financial truth. Old success cannot revive
a terminal R; a new refund requires a new provider refund ID and request key.

Emit structured refund_compensation_started/completed/conflict/retry plus the
existing reconciliation warning, with bounded reason codes and opaque IDs.
Metrics: open-case count/oldest age, retry exhaustion, reversal conflict,
compensated amount by currency, and provider-vs-ledger balance discrepancy.
Page on invariant/conflict failures; unresolved aging follows an owned runbook.
No raw payload, personal reason, bank details, credential or unbounded provider
text in logs/cases/metrics; financial references survive privacy scrubbing.

## Acceptance gate and implementation sequencing

This document is a proposal, not runtime acceptance. Its gate is docs-only diff
and green required GitHub CI at the exact source SHA; runtime scenarios are NOT_RUN.
After approval: freeze ports/ACLs and attempt identity first; implement one atomic
cross-module finalizer second; then recovery/operator/sandbox acceptance. Four
owners are inseparable for the final transaction, so split preparatory contracts
from implementation rather than shipping partial financial compensation.

Implementation tests must first fail in GitHub CI (tests-only commit), then pass
on the fixing source; record both SHAs/run IDs. No local tests/builds/containers.

1. Real PG: succeeded journal → failed and canceled; exactly one swapped Journal,
   immutable originals, C reduced once, RMA compensated, unchanged stock/quantity,
   frozen allocation and durable case. Both original Returns and direct refunds.
2. Duplicate IDs, distinct updated/failed IDs, failed then canceled, reversed
   arrival, stale succeeded, missing metadata and recovery concurrent with webhook;
   conflicting account/tenant/currency/amount/journal binding must change no money.
3. Partial/full/multiple RMA plus direct history, zero-money RMA, replacement
   attempt, repeated compensation/retry; exact remaining entitlement and rounding.
   State projection covers paid/partial/full fulfillment, never cancel or restock.
4. Advisory-lock + pg_stat_activity synchronized races: compensation vs refund
   reserve/complete/legacy/new RMA/second compensation, RC/RR/SERIALIZABLE; real
   parent UPDATE prevents stale aggregate reads. No sleep-based ordering.
5. Fault at each Journal/projection/case/outbox/HTTP receipt boundary: full rollback
   then crash recovery yields one commit. ACL/RLS negatives include runtime,
   Returns coordinator, cross-tenant and owner-role direct mutation attempts.
6. Fresh/populated forward upgrade, legacy ambiguous binding refusal, privacy
   scrub recovery; check_all/PG/races/upgrade and affected end-to-end lanes in CI.
   Add SQL files to both runner lists; discover migration by suffix/glob.
7. At least two temporary draft CI mutation probes: remove transition uniqueness
   (duplicates must fail), and restore lock-only parent fence or omit C rollback
   (race/conservation must fail). Retain run IDs; remove owned probe branches.
8. Sandbox lifecycle: actual Stripe test capture → partial/full refund → signed
   replay with persisted balanced journals, then supported test-mode failure or
   cancellation and recovery. Record API version, account mode and raw receipts.
   If Stripe cannot produce post-success terminal change in that sandbox, run a
   clearly labeled signed synthetic transition against real PG separately; mark
   real provider post-success lifecycle NOT_RUN, never call synthetic E2E proof.
   Live enablement stays blocked until orchestrator accepts the gap and the
   operational reconciliation/balance runbook. Production/live keys need approval.

## Rejected alternatives and approval questions

- Edit/delete the success Journal: destroys audit history and violates Ledger.
- Returns or operator posts a reversal independently: two money authorities race.
- Per-event or separate failed/canceled reversal keys: duplicate compensation.
- Release on timeout/error code/NoSideEffect after success: not proof of reversal.
- Keep refunded terminal and only log a warning forever: leaves false money and
  permanently stranded capacity; #292 is a safe interim refusal, not live closure.
- Reset RMA to received and rerun allocation/restock: duplicates goods or money.
- Automatically re-refund after compensation: creates a new external money action
  without fresh authorization; replacement must follow normal admission.

Approval requested: net meaning of refunded_minor and controlled state exceptions;
append-only attempt/projection model; Payments capability transfer and C-19c-1
integration order; named operator/runbook owner and treatment of any sandbox gap.
Until approved and implemented, none of these proposed exceptions is executable.
