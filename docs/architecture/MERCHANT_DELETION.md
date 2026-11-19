# Automated merchant deletion

Status: implementation contract, 2026-10-26; **not implemented by this document**.
Authority: owner's MDEL-DESIGN card (2026-10-26), replacing the support-only merchant-deletion item in [#243](https://github.com/luogangan7-lgtm/opensite-commerce/issues/243).
Automated deletion is required before production launch; a support runbook is insufficient.
The owner's 2026-10-22 financial-retention decision still applies. Baseline read: `c1b8c71efa56c9ffb948b811fe32e91a72ce5ea8`, [V1 §VI/IX](V1_DECISIONS.md),
[A1–A7](APP_LAYER_DECISIONS.md), [customer data rights](DATA_RIGHTS.md),
[`modules/privacy`](../../modules/privacy/README.md), and
[serving projection](ADR_STOREFRONT_SERVING.md). Existing customer erasure/export,
`datarights` file Sweep, local SMTP and immutable financial records remain the starting contracts.

## 1. Authority, state and admission

Tenant owns the tenant lifecycle/write fence; DataRights owns deletion requests,
the versioned execution worklist, export manifests and append-only erasure receipts.
Each module owns its tables and erasure handler; bootstrap composes their public ports into DataRights. No module writes another module's private store.
New records use `(tenant_id, id)` keys, composite FKs, minimal grants and ENABLE + FORCE RLS using `nullif(current_setting('app.tenant_id',true),'')::uuid`.
Global Identity tables retain their existing isolation contract, not invented tenant columns.

| Transition | Authority and condition |
| --- | --- |
| active → requested → cooling_off | Current active **owner** with fresh step-up; both request events and offline fence commit atomically. Export build is asynchronous. |
| cooling_off → canceled | Current owner with fresh step-up, DB time strictly before `execute_after`, no destructive work begun. Restore service atomically as below. |
| cooling_off → executing | Restricted signed worker only, DB time at/after deadline, export and obligation gates satisfied. |
| executing → deleted | Worker only, all worklist items verified and final registry/file scan clean. |
| cooling_off/executing → failed | Worker persists retryable reason, phase, attempt, next retry and lease; tenant remains offline. |
| failed → cooling_off/executing | Worker retries the saved phase with all gates rechecked; never restarts the 30 days. A pre-execution failure remains cancelable only before the original deadline. |

DB `clock_timestamp()` is sampled **after acquiring the fence** for admission,
deadlines and leases. `requested_at` and `execute_after = requested_at + interval
'720 hours'` are persisted timestamptz values: exactly 30 × 24 hours, independent
of timezone/DST and application clocks. At equality cancellation loses; lock/CAS
serialization decides cancel versus execution, not HTTP arrival time. One nonterminal request per tenant; canceled requests remain receipts. Request,
cancel and export creation require `Idempotency-Key`, bound to trusted tenant,
actor, operation and body digest. Same key/body replays metadata, changed body
conflicts; another key cannot create a second active deletion. No token/PII replay bodies; export replays return metadata only, a fresh key mints a replacement capability and revokes the previous one.

Request/cancel require an AdminSession, exact Origin, session-bound CSRF and a
single-use step-up proof from password reauthentication through Identity's existing
rate limits. Identity stores only a proof digest bound to session, user, tenant,
action and DB expiry (5 minutes); consume it in the transition transaction.
An idempotent replay still requires a live authorized session but need not reuse the consumed proof. Password/session revocation invalidates outstanding proofs.
API keys, admin, other staff, customers and support cannot request or cancel. Freeze owner transfer/removal while nonterminal; emergency suspension denies
interactive access but does not silently cancel the durable worker request.

## 2. Cooling-off and unresolved obligations

**The storefront goes offline immediately at request commit.** Keep immutable
release pointers as reversible metadata; a tenant-deletion fence denies hostname
resolution, all public/historic release and direct media reads, cart/checkout and
publish/restore/refresh. Advance publication/cache epochs; no fallback body or
cached positive host lookup may bypass the fence. Already delivered bytes cannot
be recalled. Cancel revalidates prior tenant/site status, rebuilds serving at the
new epoch and restores only previously online sites; never resurrect an older
operator suspension. If rebuilding fails, cancel rolls back and can be retried.
Staff access is read-only. Owner cancel/export/step-up, lifecycle audit and restricted workers below are explicit exceptions, not ordinary staff writes.

Request admission and execution admission both require **zero**:

- paid orders with any unfulfilled quantity, open fulfillment/return obligation;
- pending/processing/uncertain refunds or payments, in-flight checkout/capture,
  unresolved refund-reconciliation cases (case without a verified resolution);
- pending payouts, unsettled provider balance obligations or applicable legal holds.

Use Orders/Fulfillment quantities, Payments refund/case resolution witnesses and Ledger posted facts through owner ports, never a UI status or elapsed timeout.
Provider payout/hold authority must be explicitly mapped before enabling the feature; absent/unknown authority is a blocker, never assumed settled. Deletion
does not issue refunds/payouts or alter posted journals to satisfy this gate.
**PAYOUT-AUTHORITY-UNMAPPED**: the default Payments provider-settlement adapter
returns unknown for every tenant; no provider payout/balance settled witness is
produced until this authority is explicitly mapped. This remains a launch blocker.
Before acceptance return 409 `tenant_deletion_blocked` with category counts,
bounded authorized object IDs and resolution links; tenant stays online. Owner
settles via the existing workflows then submits again. During cooling-off only
already-authorized financial completion/reconciliation and privacy erasure workers
may drain existing work, never create new obligations; staff must cancel first to
perform a new business command. A newly discovered obligation at expiry persists
`failed`, `phase=preconditions`, blocked details and retry time; no erase starts.
The owner sees deadline, export state, phase, blocker counts and last/next retry.

## 3. Locks, registry and resumable execution

Every mutation entry (HTTP, SQL capability/trigger, legacy/bulk import, checkout,
webhook, River/outbox, publish/refresh and storage builder) first takes the same
tenant lifecycle row fence: `FOR SHARE` for admitted work, `FOR UPDATE` for transitions. Recheck lifecycle **inside** that transaction. Request waits for prior writers,
checks obligations, fences future writers and commits offline state together.
Execution waits for admitted drains, rechecks obligations and revokes drains
before destructive work. Retained financial ingress after that point may update
only registered opaque financial cells, never fan out PII or reopen business access.
Do not hold a DB transaction across provider/SMTP network calls; fenced admission and completion tokens prevent stale workers from recreating erased copies.

Lock order: tenant fence → export/artifact advisory locks (UUID order) → existing
module lock orders. Customer erasure keeps order → customer → request ordering;
notifications keep dispatch → envelope ordering and NOWAIT second scans. Take
one module work item per transaction, never nest conflicting module row locks.
55P03/40P01/40001 retry from the durable checkpoint with bounded backoff.

Extend the **same** `modules/privacy/*.json` registry with mandatory
`merchant_deletion` per-column policy: explicit `retain`/`erase`, reason, exact
scrub/delete strategy, tenant ownership path and responsible handler. A customer
`retain` policy is not a merchant decision. Catalog reconciliation covers every
tenant-scoped column, tenant root, indirect/global and queue envelopes, structured
payloads and artifact declarations; each exclusion needs an explicit ownership reason.
No name-based classification or arbitrary JSON-provided tenant authority. Retain only opaque financial obligations (provider refs, bigint minor amounts,
char(3) currency, states, timestamps, IDs and necessary relational witnesses),
plus explicitly classified minimal deletion-control/audit/tombstone metadata.
Every retained column needs its own reason; free-text codes must be fixed enums.
Erase names, tenant name, contacts, addresses, credentials, notes, return reasons,
arbitrary ledger labels and all derived copies, even when currently "operational".
Preserve balance/conservation, provider recovery keys and FK closure; a nonfinancial
parent needed by a retained FK becomes an ID-only scrubbed witness, not retained PII.

Ordered worklist (registry version + request + tenant + handler + cursor + lease
epoch, unique per item); module-local FK order is declared by each handler:

1. Persist executing/fence epoch and complete artifact/object inventory; revoke
   downloads, public reads, sessions' target-tenant authority and API keys. Pause
   old jobs/dispatch admission; fence admitted builders before inventory finalization.
2. Remove serving/search/cache copies and pending notification/outbox/River/webhook
   payloads; scrub historical payload/response/audit copies via registered exact
   exceptions. Preserve only classified opaque receipts. New lifecycle intents
   contain IDs/state only. No stale job may republish or refill a cache.
3. Run owner handlers in registered dependency order: leaf copies before parents,
   financial scrubs before nonfinancial parent removal. Customer/guest/inquiry,
   B2B, cart/checkout, orders/quotes, fulfillment/returns, catalog/CMS/site,
   inventory/pricing/tax/shipping, payments/ledger and identity are all covered.
   Immutable release manifests, documents, pins and revisions are erased through
   a narrowly authorized privacy executor/guard, never by disabling triggers.
4. Remove tenant media originals, variants, upload staging, customer/merchant
   exports and temporary artifacts. Use canonical IDs under anchored LocalFS roots,
   reject symlinks/traversal, prove tenant ownership before unlink. Current LocalFS
   shares content-addressed bytes across tenants: **before activation**, Media must
   move to tenant-private namespaces, copying/verifying/fsyncing each owner's bytes
   before switching references and reclaiming unreferenced shared originals.
   This separate storage upgrade preserves hashes/sealed content and all readers;
   deletion itself touches only its tenant namespace. No global hash-key unlink.
   Sync each changed parent directory (and removed directory ancestors), retry fsync even after
   ENOENT; verify inventory and an anchored tenant namespace scan are empty.
5. Detach all memberships, erase tenant customer sessions/tokens and key hashes.
   Global staff users/passwords/sessions belong to Identity and may serve other
   tenants: revoke **target-tenant authorization**, not other tenants' sessions.
   Keep their global account; remove tenant-owned profile/contact copies. Only the
   frozen requester ID may access the terminal metadata receipt after detachment.
6. Verify registry allowlist, canary absence, zero artifacts and no active writers;
   tombstone `tenants` with retained ID/status/time and erased name. Retained
   financial FKs keep that tenant ID. Commit deleted + append-only audit receipt.

Record DB mutations and checkpoint in one transaction. Files/cache IO use a committed intent then IO/verification then checkpoint CAS; crash between unlink
and checkpoint is safe. A leased worker cannot acknowledge another epoch's work.
The periodic signed DataRights sweep discovers all nonterminal requests and
unfinished artifacts from DB worklists, including deleted tenants removed from
`JOB_TENANTS`; River is a wakeup, not the source of truth. Failures never mark
deleted; retry uses durable cursors. RLS **and** explicit tenant predicates apply
to every item; worker role has no BYPASSRLS or general raw-table access.
Audit retains only IDs, DB times, registry version, counts, state/reason enums and verification digests. Backup restore replays the separate erasure journal and
artifact/cache fences before any public/admin traffic; old backup is not acceptance.

## 4. Merchant export and notifications

Define a merchant export, reusing DataRights private-file, hash, single-use POST download and Sweep primitives; do not loop customer exports (they omit merchant,
unlinked guest, inquiry and published copies). Include all tenant-owned business
data and media plus schema/retention manifest; exclude secrets, PHC/token hashes
and other tenants' or global staff account data. Use one bounded REPEATABLE READ
snapshot, record its DB time; referenced media bytes are pinned for that build.
Limits: 1 GiB total, 1,000,000 records, bounded 10 MiB/10,000-record chunks, one
private archive per request. Exceeding limits fails visibly without partial-ready;
larger stores need an approved bounded streaming export card before activation.
Build window 24 hours; retry failed builds while cooling. Archives expire at the
execution deadline (overdue rebuilds: 24 hours); rebuild expired copies before execution. Capabilities expire after at most 15 DB-clock minutes,
are hashed in DB, owner/session/tenant-bound and consumed before returning bytes.
GET/HEAD never consume. After a lost response owner can rebuild/reissue; no raw
capability in logs, URLs, outbox or idempotency storage. Status only gives metadata;
authorized export POST returns the capability. Every erasure invalidates affected
archives and triggers a rebuild. Execution requires a current complete export was
made available to owner (download not required), then revokes and deletes **all**
archives. Cancel revokes them and enqueues cleanup; no indefinite export archive.

Each state transition and export-ready/block/failure creates an idempotent
notification intent keyed by request + transition sequence. Resolve the verified
requester's global Identity contact at send time; persist no new tenant contact
copy. Deleted notification carries only receipt metadata, never a download link.
UI status remains authoritative if delivery fails. Use Notifications' existing local SMTP restrictions only (loopback, safe test domains, nonproduction); do not
relax destination guards or enable an external sender. Fake/loopback CI proves
notifications; real owner email delivery remains unavailable until separately
approved. Sender approval is an additional production-launch prerequisite.

## 5. Authz and future OpenAPI shape

Add `tenant:delete` for **owner only**, with an explicit role check as well as scope.
Atomically synchronize Go RoleScopes, DB `auth_role_scopes` (forward SQL wrapper capturing the previous
function expression and returning its full result plus only the new owner grant;
assert new scopes ⊇ old scopes for every role, preserving owner and ACL), API-key finite scope enum, OpenAPI enum/x-required-scope and exact
gate tables. Declaring it in the key enum grants no key access: all routes below
are AdminSession-only. Unknown scopes remain denied. No OpenAPI edit in this docs PR.

Prefix `/api/v1/t/{tenant_id}/merchant-deletion`; every operation uses shared
`Unauthorized` **401**, `Forbidden` 403, `NotFound` 404 and redacted server errors.
Mutations share validation/409 conflict/429 responses and CSRF; all responses use no-store.

| Method/path suffix | Contract |
| --- | --- |
| POST `/step-up` | owner password reauthentication bound to request/cancel action; 200 one-use proof, existing Identity rate limits |
| POST (root) | request; step-up proof + Idempotency-Key; 202 deletion metadata, 409 blocked details |
| GET `/{request_id}` | status; current owner, or authenticated frozen requester for terminal receipt only; no export token/contact |
| POST `/{request_id}/cancel` | cancel; step-up + Idempotency-Key; 200 canceled metadata, 409 deadline/phase conflict |
| POST `/{request_id}/export` | create/rebuild or issue a one-use capability; owner + Idempotency-Key; 202 building or 200 capability (never replay the secret) |
| POST `/{request_id}/export/download` | owner + capability in body; 200 archive, 409 consumed/revoked/expired; no GET download |

## 6. CI acceptance and implementation order

This PR's gate is current-head documentation-compatible CI only; no runtime
completion claim. Implementation remains disabled until **all** following gates
are green at final SHA. CI-only Go/PG18.6 gates use pending stable-id migrations,
exact role matrices and populated upgrades, never hardcoded migration numbers.
Each behavior card pushes tests-only RED then implementation GREEN and records
both run IDs. At least two isolated CI mutation probes must fail at assertions:
omit a tenant predicate/fence, and skip a derived/file scrub. Races synchronize
with advisory locks + `pg_stat_activity`, never sleep ordering.

| Ordered card / one behavior module | Required gate (CI) |
| --- | --- |
| MDEL-00 DataRights contract | Registry schema/handler dependency contract; every actual tenant column/artifact classified bidirectionally; unknown/missing reason/cycle fails. |
| MDEL-01 Tenant | Lifecycle, common fence, obligation port and owner-only scope contract sync; request/cancel/deadline/replay, missing GUC, exact grants, write/drain races. |
| MDEL-02 Identity | Step-up, session/key fencing, terminal receipt exception; admin/key/customer/CSRF/stale proof/cross-tenant denial; global other-tenant session survives. |
| MDEL-03 Orders | Obligation witness and scrub; paid-unfulfilled blocks, fulfillment boundary race, retained money/ID-only FK witnesses. |
| MDEL-04 Payments | Pending refund/payment/payout and unresolved case blockers; ingress fencing, settled witness, no duplicate money effect. |
| MDEL-05 Ledger | Exact retained-column reasons, immutable balanced posted journals and fixed-enum/no-free-text retained cells. |
| MDEL-06 Fulfillment | Leaf scrubs and pending quantity witness; no late tracking/contact recreation. |
| MDEL-07 Returns | Reason/response/restock-derived scrub; frozen refund authority and recovery unchanged. |
| MDEL-08 Checkout | Session/attempt/idempotency scrub and capture fencing; late completion cannot recreate PII. |
| MDEL-09 Customer | All tenant customers, guests/proof links/credentials erased; customer-erasure race preserves tenant isolation. |
| MDEL-10 Quotes | Inquiry/accepted/unaccepted/snapshot/receipt copies erased; tenant scope never email matching. |
| MDEL-11 B2B | Buyer/company/member/contact cleanup and financial witness FK closure. |
| MDEL-12 Cart | Saved/anonymous carts and payload cleanup; concurrent write denied. |
| MDEL-13 Publish | Current/historic release, serving/fallback/cache/refresh fences; cancel rebuild restores service; immutable guard negative controls. |
| MDEL-14 CMS | Draft/revision/template/document scrubs, including unpublished content. |
| MDEL-15 Catalog | Product/variant/content/import receipt cleanup; bulk/legacy writes fenced. |
| MDEL-16 Inventory | Movement free text scrub, retained quantity/financial witnesses unchanged. |
| MDEL-17 Pricing | Price-list text/config cleanup and serving rebuild cannot refill deleted tenant. |
| MDEL-18 Tax | Rules/contact/snapshots classified and erased; only registered financial facts survive. |
| MDEL-19 Shipping | Zone/method/contact/free-text cleanup and no late derived copy. |
| MDEL-20 Site | Domains/site metadata detached; every host/direct resource denied, other tenants untouched. |
| MDEL-21a Media storage prerequisite | Populated shared-blob→tenant-private upgrade; hash/reader/pin integrity, crash/resume and concurrent upload; no shared legacy bytes before activation. |
| MDEL-21b Media erasure | Anchored original/variant/staging unlink, parent fsync faults/retry, namespace empty; symlink/cross-tenant negatives. |
| MDEL-22 Reliability | Inbox/outbox/jobs/idempotency/audit payload cleanup; crash/stale lease/replay cannot recreate PII. |
| MDEL-23 Notifications | All delivery/redrive generations scrubbed; transition intents deduped; fake/local SMTP only, no late PII fanout. |
| MDEL-24 DataRights | Bounded merchant export + durable sweep/coordinator; crash/resume before/after **every** item and IO checkpoint, DB-clock expiry, consumed-token denial. |
| MDEL-25 Admin UI | CI browser owner request→cancel (storefront restored) and request→DB-clock-advanced→deleted; blockers/export/status, admin denied. |
| MDEL-26 Bootstrap | Full populated all-module real-PG lifecycle: retained allowlist only + minimal receipt/tombstone, zero personal canaries/files, cross-tenant byte-identical witness, replay and backup-journal restore; populated forward-upgrade and all affected E2E modes. |

Behavior cards depend on 00–02; execution's explicit dependency graph places copy cleanup before FK parents (table order above is delivery order, not SQL order).
Required shared contract surfaces travel with their owning behavior card; this is
not permission for unrelated cross-module implementation. Request activation waits
for 26 and sender approval; statutory hold/retention durations and unsupported
provider payout authority remain explicit launch blockers, not guessed policies.
