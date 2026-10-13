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

No `privacy_exports` table or export-temp store is present in this base. When #44
adds its request metadata, export artifacts or filesystem staging, it must declare
the new database columns here and document/classify the artifact bytes and
retention path. A response-body/file reference declaration is not coverage of the
referenced artifact bytes. Media blob content also requires its owning storage
module's artifact policy; this database gate covers filenames and stored fields,
not OCR, image pixels, provider systems, logs or backups.

Only GitHub CI runs database gates. Local tests, gates, containers and builds are
NOT_RUN under the owner's CI-only rule. Removing this contract is a code-only
rollback and does not alter a database migration or business write path.
