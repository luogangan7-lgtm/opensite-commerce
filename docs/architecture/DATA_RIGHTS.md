# Data rights and catalog CSV (C-22a)

This describes the local implementation contract, not a legal retention policy.
The `datarights` module owns export requests and erasure receipts. Customer,
orders, cart, ledger, catalog and publish retain their existing table ownership.

## Admission and export lifecycle

All four customer routes require `AdminOrAPIKey` and `customers:privacy`.
Catalog CSV routes require `catalog:import` or `catalog:export`. The platform
authenticator must verify the URL tenant and scope, and its trusted subject
resolver supplies the actor. Neither an actor header nor a URL tenant is proof
of authority. Missing actor integration denies access by default.

Export request creation and erasure require an `Idempotency-Key`. The claim,
domain writes and signed River enqueue share the authorized transaction. Saved
responses contain request metadata only. JSON files, download capabilities and
erased PII are never saved as idempotency response bodies.

The signed `customer_data_export` job binds the trusted tenant and request UUID.
The worker uses its separate controlled runtime runner to build one customer's
profile, addresses, order snapshot summaries and carts. Credential hashes are
excluded. An export has a 24-hour processing window and bounded content size
(10 MiB, at most 10,000 child records). Downloads have a separate capability
deadline of at most 15 minutes, capped by the request deadline.

Files live below `OPENSITE_MEDIA_ROOT/data-rights/<tenant UUID>/<request UUID>.json`.
Only canonical server IDs determine paths. The directory is private and files
use mode 0600; writes use atomic replacement. The filesystem shares the media
storage root but exports are never public media assets.

Authorized status reads derive the capability with a server HMAC key. The
database stores its hash and deadline, never the raw capability. Download
requires both scope admission and the capability, checks file integrity and
consumes the request while customer/request locks are held. An interrupted
network response still consumes the link; request another export. Concurrent
downloads cannot both consume it. Expired, consumed or revoked links fail.

## Erasure and restore

API and export-worker processes use the same absolute `OPENSITE_MEDIA_ROOT`.
The former `OPENSITE_PRIVACY_ROOT` alias, if supplied, must resolve to that same
configured path or startup fails before a database connection is opened.
`OPENSITE_PRIVACY_DATABASE_URL` and `PRIVACY_WORKER_DATABASE_URL` use a restricted
runtime login; the queue worker login has no customer-data permissions.
`PRIVACY_TOKEN_KEY` and `JOB_SIGNING_KEY` are hexadecimal environment secrets
shared by the relevant API/worker processes. Rotating the token key invalidates
outstanding links; rotating the job key requires draining or re-enqueuing old
signed jobs. `PRIVACY_EXPORT_WORKER_ENABLED=true` enables API enqueueing only
after the worker is configured. Identity admission still defaults to denial
until the host supplies a trusted subject resolver.

Erasure replaces customer name/email with irreversible placeholders, disables
the customer, deletes addresses and password credentials, revokes company
membership, scrubs order shipping/tax JSON and revokes every outstanding export.
Amounts, currency, quantities, order IDs/state and ledger entries remain intact.
Snapshot guards permit only the exact privacy scrub under an unassumable,
non-BYPASSRLS executor role; runtime cannot use that exception directly.

Export generation/download lock the customer before their request. Erasure first
locks existing customer orders in UUID order, then the customer and requests. A
second order scan uses NOWAIT to catch newly committed orders without waiting on
a financial transition that needs the customer lock. This serializes erasure
with export generation/download and notification fanout. Guards prevent new credentials, addresses or customer orders from
resurrecting PII for an erased customer. Retry of erasure is safe; an old export
job cannot make a revoked request ready again. Files are cleaned by ID after
database erasure. If unlink fails, access remains revoked; retry cleanup before
retiring the storage volume. Pending and expired artifacts also require storage
retention cleanup; SQL deadlines alone do not delete physical files.

Order notifications are owned through `(tenant_id, orders.customer_id)` and
`reference_id`, never through email equality. Erasure replaces their frozen
recipients with `erased-<delivery UUID>@invalid.test`, clears send tokens, and
retires every pending/current/redrive dispatch generation. Already delivered
outbox receipts retain their historical delivered state. The contact and
terminal dispatch guards permit only the exact scrub under the privacy executor;
redrive cannot revive an erased envelope. Later order lifecycle events still
commit, but their notifications and dispatches start terminal and contain no
original contact. Guest orders with no customer link remain outside this scope.

Notification workers admit a send in a committed database transaction before
SMTP IO. Erasure prevents subsequent admission and stale completion, but cannot
recall a message whose send was already admitted or accepted externally. Real
SMTP cancellation/delivery is **NOT_RUN**; the DB tests cover token/lease fencing,
recipient removal and future fanout. Customer sessions and future quote/inquiry
ownership do not gain an invented mapping from this integration.

Erasure locks existing dispatches before envelopes. A concurrent redrive may
introduce a generation while these locks are acquired; NOWAIT on envelope locks
and the second dispatch scan aborts the entire operation on contention. Retry
`privacy_busy` with the same idempotency key. SQLSTATE 55P03, 40P01 and 40001
from domain work map to the stable HTTP 409 code `privacy_busy`; rollback saves
no successful response. Errors opening or committing the outer authorized
transaction still use the redacted HTTP 500 response; this mapping does not
claim to classify every transaction failure.

The append-only erasure receipt and audit event record tenant/customer ID,
trusted actor, time and affected categories/counts, without erased PII. Keep
the current erasure journal separately from old backups. After restoring an
older database, replay later journal customer IDs through the authorized erase
service (fresh idempotency keys), clean export artifacts and verify ordinary
queries before enabling application access. Restoring an old backup alone
does not establish that later erasures were applied. This card does not automate
backup-journal replication or choose statutory retention periods.

## Sealed release media and customer sessions

The current schema has no customer-owned avatar/comment media relationship and
no customer-session table. `media_uploads.actor_id` is an uploader identifier,
not a customer ownership contract; staff users must not be matched by email.
Credential deletion and company-member revocation are exercised. Customer
session revocation and customer-media tombstones are **NOT_APPLICABLE** to this
schema, and their future runtime paths are **NOT_RUN**. Existing sealed release
media pinning remains unchanged and is covered by the privacy SQL regression.

When customer-owned media is introduced, its public contract must first bind
tenant/customer/asset with composite foreign keys. Erasure should append an
immutable, tenant-scoped tombstone for that association, revoke original-content
delivery, and let release reads render a fixed placeholder before blob access.
The sealed release manifest/pins and original hashes stay immutable. Garbage
collection can then remove original bytes after readers are fenced, while the
placeholder is application-owned and carries no original metadata. Tests must
cover live and historic sealed reads, direct content access, concurrent erasure,
tenant isolation and backup replay before declaring that path implemented.

## CSV batch contract

The UTF-8 header is `product_code,title,sku,locale`. A blank SKU imports a product
without a variant. Locale is explicit; configured-option variants and archived
rows are rejected rather than guessing an option tuple. Existing product/SKU
IDs are retained. A changed locale title creates a content revision; imports
do not change published release pointers or overwrite body/SEO content.

Preflight collects row errors and performs no writes. Import performs preflight
inside the same transaction as the writes: any conflict or SQL error rolls the
whole batch back. Import validation failures return a structured report with
zero imported rows (HTTP 200); HTTP status alone does not indicate a successful
batch. Request size is at most 1 MiB and 1,000 data records.
Exports prefix spreadsheet formula cells beginning with `=`, `+`, `-` or `@`
with an apostrophe and use standard CSV quoting. The importer handles this
export escape for round trips. This is not a general arbitrary-schema importer.

Focused red/green probes and full Go/PG gate receipts are listed in the PR body;
production identity wiring and external storage delivery are outside this card.
