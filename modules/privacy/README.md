# Persistent-field privacy contract

The JSON files here are owned by the existing modules in `modules/registry.yaml`.
They classify every column of every **base table in schema `app`**, and the pinned
River `queue.river_job` envelope. `public.schema_migrations`, other River internal
bookkeeping tables, views, and nonpersistent HTTP bodies are outside this catalog
scope. Table ownership remains in the module registry; it is not duplicated here.

The gate never infers privacy from a column's name. Owners must review the actual
meaning and write paths when classifying a new field. `personal` includes free
text and structured copies that may contain identifying data. `contact` covers
email/contact/address text. `financial_non_contact` is amounts, currency and
financial bookkeeping without contact text. `operational` is relational keys,
state and nonpersonal configuration. `secret_hash` is PHC/digest/capability hashes
that must not be exported as profile data. This classification is an engineering
contract, not a legal retention decision.

A personal/contact field declares a subject (inherited from its table or overridden
on the field), erasure strategy and export treatment. `tenant_key: null` is explicit
for global staff identity and vendor job records. `identity_path` is a readable
ownership path, **never executable SQL**. `null_subject_policy` records ambiguous
or unregistered owners; matching an email in an arbitrary payload does not prove
ownership. Each JSON/JSONB/bytea field, including arrays, names a structure defined
in its module file. Conservative structured payload declarations apply to every
producer, including legacy, bulk and asynchronous writers.

`erase.strategy` is one of scrub/null/placeholder/retain; retain requires a reason.
`export` is include/redact/exclude. These are obligations for the owner module;
**this card implements no erasure or export engine**. In particular, declaring
checkout snapshot scrubbing does not bypass migration 032's immutability trigger.
Data-rights #44 must provide the controlled change and its own end-to-end gate.
Staff anti-abuse evidence and public merchant contact/publication history have
explicit retain reasons; those records belong to different subjects from a
registered storefront customer and must not be erased by guessing an email.

## Merchant deletion registry v1 (MDEL-00)

Authority: [MERCHANT_DELETION §§1–5 and §6 MDEL-00](../../docs/architecture/MERCHANT_DELETION.md).
The same JSON files now carry a **separate mandatory `merchant_deletion` policy
on every column**, including operational fields, generated columns, structured
payloads, the tenant root, global Identity/anti-abuse tables and River envelopes.
A customer retain reason never supplies a merchant policy. These declarations
are handler obligations, **not implemented handlers or permission to activate
merchant deletion**. No new route, scope, request transition or DB grant exists.
Activation still waits for MDEL-26 and sender approval. Applicable legal holds
and provider payout authority remain launch gates. The owner approved a
configurable financial-retention duration (default 10 years from merchant
deletion), followed by automatic purge, on 2026-10-26. This registry declares
retained cells; it does not implement that future retention/purge engine.

Each column supplies `action` (`retain`/`erase`), its own nonblank `reason`, an
exact `strategy`, its owning `handler` and an explicit `ownership`:

- `tenant_column`: the real same-table tenant key (`tenants.id` for the root).
  The gate validates that key against the same catalog-backed registry. Both
  RLS and an explicit trusted-tenant predicate are required of future handlers.
- `global_exclusion`: an explicit ownership reason for a table without a tenant
  key. Global staff accounts, passwords/sessions and global anti-abuse evidence
  survive; Identity revokes only target-tenant authorization. This is an exclusion
  from merchant ownership, not financial retention of tenant contact data.
- `signed_job`: only the pinned River envelope, attributed through
  `verified_signed_job_tenant`. Reliability must verify its signed trusted job
  binding; arbitrary JSON tenant fields never authorize classification/deletion.
  Unbound/system jobs and other tenants' jobs are outside that ownership path.

`retain` requires `unchanged`, an explicit retained kind
(`financial`/`witness`/`control`/`global`) and a value type. Fixed enums enumerate
their values. Tenant personal/contact fields cannot be retained; frozen financial
request/evidence digests are distinct from credentials. `FINANCIAL_DIGESTS` explicitly lists the reviewed integrity columns; declaring an API-key/password hash as a financial digest never permits retention. Financial references are
opaque provider/recovery identities under DATA_RIGHTS, never customer-entered
notes. Conservation amounts, currencies, states, timestamps, IDs and necessary
FK witnesses survive. Nonfinancial parent payloads are scrubbed; only referenced
IDs/relationships remain original. Settlement currency remains solely because
retained financial composite FKs require it. The owner-approved financial-retention duration applies to those witnesses.

Nullable financial receipt cells explicitly set `preserve_null: true`: leave
an original null unchanged and replace only an existing value. In particular,
registered orders must keep `guest_contact IS NULL`, and pending Checkout
receipts must not invent a nonnull start/final result. This flag is allowed only
for constant/row-ID/row-JSON scrubs, not retention or artifact deletion.

Erasure strategies are declarative, never executable SQL from JSON:

| Strategy | Exact owner-handler obligation |
| --- | --- |
| `delete_row` | Delete the owned row in the declared local leaf-before-parent order. All columns assigned to that same work item must agree on row deletion. |
| `constant` | Replace with the exact JSON `value`, including explicit null; scrub guards must admit only that change. |
| `row_id_placeholder` | Substitute only the row's canonical UUID into the declared `{id}` template. Preserve uniqueness, never interpolate original text. |
| `row_json_template` | Build the declared flat JSON object using only fixed scalars, `{id}` UUID substitution, and `{"column": "name"}` references to same-row retained financial integer/currency facts. Never copy original contact objects or execute SQL. Orders guest contact uses `erased-<row UUID>@invalid.test` (preserving NULL); shipping keeps `shipping_minor` as `amount_minor` and the original `currency`, matching the unchanged privacy trigger. |
| `database_time` | Replace the original nonfinancial timestamp with the DB erasure time sampled after the tenant fence. |
| `generated_from` | Scrub the named same-table source; let the stored generated expression recompute. Never write a generated column. |
| `fixed_enum` | Keep an enumerated fixed code; replace any other text with the declared enum fallback. |
| `actor_placeholder` | For Checkout only: replace guest authority with the declared fixed digest, customer authority with the checkout row UUID; revoke original authority. |
| `financial_account_code` | Ledger only: keep `revenue`, `tax_payable`, `shipping_revenue`, `cash`; replace every other label with `erased:<account UUID>`. |
| `financial_source_id` | Ledger only: keep a canonical source ID proved by the matching Orders/Returns/Payments financial witness; otherwise use `erased:<journal UUID>`. |
| `financial_event` | Ledger only: keep `refund`, `posted_to_compensated/v1`, or `checkout:<64 lowercase hex>` only with its matching financial witness; otherwise use `erased:<journal UUID>`. |
| `unlink_tenant_namespace` | Revoke access, inventory canonical owned IDs (including orphans), unlink under an anchored root without symlinks, fsync changed parents/ancestors even on ENOENT retry, verify empty. |

The three financial-label strategies erase arbitrary free text while preserving
registered fixed financial roles/recovery identities. Their witnesses are the
existing Orders/Returns/Payments-to-Ledger bindings, not string shape alone.
Owner cards must implement narrowly authorized guards for immutable financial,
release and receipt rows; declaring these strategies never disables a trigger.

Each module's top-level `merchant_deletion` has version 1, `handlers`, and
`artifacts` (explicitly empty where none exist). Handler IDs are module-qualified;
`tables` is its complete ordered module-local worklist for the explicitly
assigned columns, not permission to access another module's store. A table may
appear in separate owner work items: `site.detach` first clears the Site-owned
`sites.published_release_id` FK, `publish.erase` can then remove release copies,
and `site.erase` removes site rows after their leaves. This avoids the existing
Site↔Publish FK cycle without cross-module writes or dropping constraints.
The pointer stays reversible during cooling-off; these items execute only after
execution admission. `depends_on` is a DAG: unknown handlers, duplicate/wrong
owners, missing table/item assignments and cycles fail. The initial dependency order cleans
serving, notifications, queue/replay and artifact copies before financial scrubs
and FK parent witnesses, then Identity and terminal DataRights/Tenant metadata.
Every handler declares DB clock, one-item mutation+checkpoint transaction, and
`tenant_fence -> artifact_uuid -> module` lock order (artifact UUIDs ascending;
existing module row-lock order remains authoritative). Each execution item must
bind registry version **and digest**, tenant/request IDs, handler, cursor and
lease epoch; acknowledgments/checkpoints must CAS that same epoch. Bootstrap
composes public owner ports; this contract adds no cross-module DML or IO engine.
No network IO may occur inside the transaction; IO intents/verification precede
checkpoint CAS, with bounded retries for 55P03/40P01/40001 in the coordinator card.

File bytes have their own policies, independently of DB storage-key columns.
The current source inventory has four kinds: media originals, upload staging,
customer export JSON and export temporary files. Explicit `privacy-artifact`
annotations distinguish temporary creation from final Rename publication at the
two filesystem writer implementations. The gate scans production
`internal/**/*.go` filesystem-create/OpenFile/Rename calls, rejects
unannotated writer calls, and reconciles artifact IDs/source paths in both
directions. OS writers must be called directly: aliases, method values and
callback/return references fail closed even if annotated. Typed dot-imported
calls are inventoried; unresolved dot imports of OS fail closed. A future
producer/kind must extend that inventory and declaration;
these checks are source reconciliation, not runtime file erasure evidence.
No variants or merchant archives exist yet: their owner cards must add actual
producers and policies together. Media's **current** originals/staging paths
are shared, so both artifact declarations require MDEL-21a; the erasure strategy
must never unlink a global hash key before the tenant-private upgrade. Existing
customer exports include final/temp/rollback-orphan copies, not just ready rows.
Serving/cache bodies and historical payloads are catalog columns, not excluded
because they are derived. External logs, backups, provider systems, schema
migration history and River bookkeeping other than the pinned envelope are
outside tenant-artifact ownership; backup restoration must still replay the
separate erasure journal/fences before traffic under §3.

`load_merchant_contract` returns the version, canonical SHA-256 content digest,
policies, artifact inventory and deterministic topological handler order. The
existing offline gate and real-PG bidirectional catalog gate call it; metadata
changes invalidate the digest. Tests reject missing/unknown policy, blank reason,
forged tenant authority, invalid retention, missing/cyclic handlers and missing or
unscrubbed artifacts. MDEL-00 now includes the pending `mdel00-caller-keys`
forward migration, HTTP UUID admission and internal UUIDv5 derivation. Its DB
CHECKs, concurrency fixtures and populated-forward-upgrade cases run in CI.
Merchant-deletion execution, orchestration and lifecycle activation remain work
of the implementing owner cards.

### Caller-key migration notes (pre-launch only)

The UUID CHECK columns in the pending migration accept only canonical lowercase
UUIDs. Surviving rows retain these random/derived identifiers unchanged as
`opaque_ref`: they contain no personal data and must never be rewritten to
`erased:{id}`. Whole-row deletion does not scrub a surviving cell. The registry
gate checks replacement compatibility against the actual UUID CHECK even if a
field omits `caller_key`; only a canonical UUID, the bare row UUID or SQL NULL
can be a replacement. Existing canonical keys remain unchanged on upgrade;
legacy non-UUID rows intentionally refuse migration.

Per the orchestrator's 2026-10-28 pre-launch decision, there are no in-flight
Checkout sessions requiring provider-key compatibility. Provider command keys
change from `checkout:v1:<row UUID>:<action>` to
`DerivedKey("checkout.provider.v1", row.ID, action)`; no compatibility shim is
introduced. This derivation change is safe only before launch. After launch,
any derivation change requires a durably stored provider key and retries must
replay that stored key. The versioned purpose, row/action tuple and pinned
provider-key vector are part of the retry contract. The authoritative launch
boundary is [V1_DECISIONS §9](../../docs/architecture/V1_DECISIONS.md#mdel-00-provider-key-derivation-pre-launch-only-2026-10-28).

## CI gates and fixture consumer

`check_all.sh` runs policy validation and the Python contract tests. The foundation
CI job runs `python3 scripts/privacy_registry.py --database --fixture` after the
real migrations/security/upgrade tests. Catalog reconciliation is bidirectional:
unknown actual columns and declarations without real columns fail. The fixture
uses real constraints, triggers and inventory/order/quote commands, DB clocks,
and parent-before-child insertion. No constraints or triggers are disabled.

The normal gate uses one transaction and **ROLLBACK**. It reads the real persisted
cell of each personal/contact column and checks its own distinct canary. A marker
in another column or a claimed coverage counter cannot satisfy that check. Each
stored generated email is exercised by a separate token carried in its source
email. A declared legal documentation address is used for the typed `inet` field.
The fixture never starts a worker or Sender. Fixed fixture IDs intentionally fail
on repeated loads in the same committed database instead of reusing stale rows.

For #44, run in an isolated GitHub CI test database after all migrations:

```sh
python3 scripts/privacy_registry.py --emit-fixture "$RUNNER_TEMP/privacy-fixture.sql" \
  --manifest "$RUNNER_TEMP/privacy-canaries.json" --prefix privacyerase
psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 -1 -f "$RUNNER_TEMP/privacy-fixture.sql"
```

The generated SQL contains no transaction terminator; `-1` commits the valid
fixture for a later erasure test. Its `privacy.fixture.tenant_id`, `customer_id`,
`checkout_id` and `quote_id` settings are transaction-local; consumers should use
fixture IDs from the SQL or obtain their identities inside the same transaction.
The manifest maps `table.column` / `queue.table.column` to exact unique canaries.
#44 must erase only its verified subject, then search the full catalog for the
subject's canaries, while checking explicit retain/other-subject witnesses. The
row graph includes registered and guest orders, checkout, quote approval, and
notification recipient copies. Registered-customer proof is established by the
real mailbox-proof command, with a distinct marker in its persisted token-email
copy and random unknown PHC material; no mail or login is performed. Retained
public/staff canaries are intentionally
separate from customer-owned canaries. A caller may reuse `fixture_values_sql` and
`validate_fixture` before erasure to assert actual coverage; classification alone
is not a claim that the data-rights executor already fulfills the policy.

Customer exports now use `data_rights_exports` and the private final/temporary
files declared in `datarights.json`. Future request metadata, archives and staging
must extend both the column registry and the independent artifact inventory. A response-body/file reference declaration is not coverage of the
referenced artifact bytes. Media blob content also requires its owning storage
module's artifact policy; this database gate covers filenames and stored fields,
not OCR, image pixels, provider systems, logs or backups.

Only GitHub CI runs database gates. Local tests, gates, containers and builds are
NOT_RUN under the owner's CI-only rule. Removing this contract is a code-only
rollback and does not alter a database migration or business write path.

The append-only `data_rights_erasures.scope` remains the existing fixed control
enum. Merchant deletion issues no UPDATE/DELETE against the erasure journal;
its immutable trigger remains authoritative. Catalog option keys use stable
`erased:{id}` replacements, preserving multiple variants and their financial FKs.

Artifact inventory uses `scripts/privacywriters` and Go AST/type information to
identify `os` and `(*os.Root)` Create/CreateTemp/WriteFile/OpenFile calls. Each
writer needs its artifact comment group immediately before that call's line;
a file-level marker cannot authorize another call. Multiple calls on that line
cannot share a marker. OpenFile wrappers retain the prior conservative inventory
regardless of receiver type; a type lookup never exempts an existing writer pattern. Standalone synthetic fixtures are scanned conservatively
by selector name. This is source inventory, not proof of runtime bytes removal.
