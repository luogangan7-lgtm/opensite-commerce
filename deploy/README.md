# Local operations rehearsal

This stack uses Docker Compose v2, Python 3.11+ and pinned PostgreSQL 18.6 and
Go 1.27.2 images. Run commands from the repository root. It exercises local
synthetic data; cloud deployment, TLS, real credentials and alerts are outside
this stack.

## Start and migrate

Copy `.env.example` to `.env`, restrict its permissions (`chmod 600 .env`) and
replace all eight password placeholders with independent random values of at
least 24 characters, the independent 64-hex job signing key, and the staff
login settings: `OPENSITE_ADMIN_ORIGIN` (the exact HTTPS origin of the admin
UI, no path) and an independent 64-hex `OPENSITE_AUTH_SIGNING_KEY` (for example
`openssl rand -hex 32`). Compose passes them to the API as `OSC_ADMIN_ORIGIN`
and `OSC_AUTH_SIGNING_KEY_HEX`; staff login reuses the auth login DSN, and the
API refuses to start if either value is missing or malformed. Customer authentication
also requires `OPENSITE_CUSTOMER_AUTH_DB_PASSWORD`, a pinned HTTPS
`OPENSITE_STOREFRONT_ORIGIN` and an independent `OPENSITE_CUSTOMER_CSRF_SECRET`
containing 32–128 CSPRNG bytes encoded with unpadded base64url. Use simple `KEY=value` or single-quoted values, with no
interpolation or multiline values. The isolated refund coordinator additionally requires
`OPENSITE_RETURNS_DB_PASSWORD`; its LOGIN receives only `app_returns_service`.
Never commit the file or print resolved
Compose configuration. The helpers read `.env`; exported variables take
precedence. Set `OPENSITE_ENV_FILE` to use another private file and
`COMPOSE_PROJECT_NAME` to isolate a stack.

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --build --wait api worker mailpit
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/api/v1/auth/login
curl --fail http://127.0.0.1:8080/metrics
curl --fail http://127.0.0.1:8081/healthz
curl --fail http://127.0.0.1:8081/metrics
```

Postgres uses a TCP readiness probe so its temporary initialization server
cannot satisfy the migration dependency. The migrator checks the cluster,
installs the locked River history in its independently owned `queue` schema,
applies frozen forward-only/checksum-checked application migrations, verifies
River history, and provisions restricted login identities. API startup waits for successful
migration and verifies its separately configured runtime, auth, domain and
media and customer-auth pools. Staff login (`/api/v1/auth/*`) is installed on the auth pool's
DSN; the domain consumer remains pending its application wiring. The separate media identity
uses the existing media capability rather than granting it to runtime.

The eighth LOGIN, `opensite_returns_login`, receives only `app_returns_service`.
Set an independent `OPENSITE_RETURNS_DB_PASSWORD` for migration/restore and API;
non-Compose API deployments may instead supply `OPENSITE_RETURNS_DATABASE_URL`.
The credential is never shared with workers or the renderer. Startup validates
its exact capability and exposes only authenticated tenant transactions at
`Config.ReturnsCoordinator.Runner`; the refund HTTP operation keeps its existing
behavior until trusted capture/account/provider bindings consume that seam.
Missing configuration names its setting in a startup log without a value.
The operations drill checks the same HBA and permission boundaries before and
after restore, including real password authentication for this identity.

Customer login (`/api/v1/t/{tenant_id}/customer/*`) uses only
`opensite_customer_auth_login` → `app_customer_auth`; it never receives staff
auth or executor capabilities. Registration/reset proof goes to the pinned
Mailpit development capture. Mailpit shares API loopback, listens for SMTP on
`127.0.0.1:1025`, and disables SMTP relay. Its capture UI/API is published only
on `127.0.0.1:${OPENSITE_MAIL_PORT:-8025}`. No SMTP port is published. This
explicit local Compose setup enables the Sender; the application default remains
disabled and refuses an enabled Sender in production. This is not a production
mail deployment recipe. All challenge recipients must be `.test`, `.invalid`
or `localhost`; never send a real customer address through this rehearsal.

Run an explicit migration/provisioning step after changing migration files:

```sh
docker compose --env-file .env -f deploy/compose.yaml run --rm migrate install
```

Only the migrator and PostgreSQL service receive the administrator password.
The worker has separate queue and outbox login pools, starts actual River
processing, and exposes health only after startup. A bounded metrics query
returns backlog, DLQ count and oldest backlog age across tenants without
returning tenant IDs or payloads. The broker owns that narrow function; workers
receive EXECUTE, without direct outbox reads or queue DDL.
API logins are `NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`, with exactly
one approved group membership each. Database `CONNECT` is revoked from PUBLIC.
Before migration, provisioning or restoration, existing application LOGINs are
audited for direct grants, RLS policy targets and owned objects across the cluster. Those remnants
cause `TAINTED_LOGIN_ROLE` before migration, database or password writes. Only
the target database's prior nongrantable `CONNECT` is accepted for a rerun.
An administrator must review and explicitly resolve the residual privileges;
provisioning never silently strips them or changes unrelated databases.
Reusing a component password, including the administrator password, is refused
before provisioning writes; each LOGIN needs an independent secret.
See [runtime-roles.md](runtime-roles.md). `/healthz` is process liveness (204),
not a continuous database-readiness probe. Compose also checks PostgreSQL
readiness. HTTP metrics count requests, errors and latency with bounded
method/status labels; they contain no path, query, customer or tenant labels.

## One application database per cluster

Before migration or restoration, preflight inspects every non-template database
as administrator. A database with connections disabled cannot be inspected and
refuses deployment. Another database containing both the
`app` schema and `public.schema_migrations` causes
`SECOND_APPLICATION_DATABASE`; failed inspection also refuses deployment.
Plain databases are permitted. V1 database names must be lowercase ASCII
identifiers (letters, numbers and underscores, maximum 63 characters).
On a fresh Compose volume, the initdb hook places exact target-database SCRAM
rules before the existing administrator rules. The eight application LOGINs are
refused connections to maintenance/plain databases and physical replication,
over Unix sockets, IPv4 and IPv6. This leaves unrelated database ACLs intact.
The hook runs only at initialization. For an existing volume, back up the
original HBA file, review equivalent rules with an administrator, reload
PostgreSQL configuration and verify password/connection refusals before use.
Changing `OPENSITE_DB` likewise requires an explicit HBA update and reload.

To inspect without changing the database:

```sh
docker compose --env-file .env -f deploy/compose.yaml run --rm migrate preflight
```

On failure, move the other application to its own cluster and rerun preflight.
Do not erase its ledger or schema to suppress the check. V1 capability roles
are cluster-wide; role-name changes cannot safely solve this during setup.
Preflight is an operational constraint for trusted provisioning, not a fence
against another administrator concurrently creating databases.

## Backup and restore

```sh
bash scripts/backup.sh --directory work/backups
bash scripts/restore.sh work/backups/opensite-TIMESTAMP.dump
```

The backup prints archive and manifest filenames. It creates private,
timestamped `pg_dump -Fc` archives and SHA-256 manifests with counts and
deterministic content hashes for every `app` and `queue` table, including customer
proof/session/credential tables and
River migration history, plus the application migration ledger.
Format 2 also records each application migration's version and SHA-256.
Dump, table hashes and migration history share one exported REPEATABLE READ snapshot. Restrict access
to both files: archives contain all application data. `OPENSITE_BACKUP_DIR`
or `--directory` changes the output location. This is a full backup, without
WAL archiving, PITR or encryption-at-rest automation.

Restore targets only the configured `OPENSITE_DB`, verifies the archive hash
and exact application migration set/checksums before touching PostgreSQL,
and creates an absent database from `template0`. Recovery uses the matching
source version: it never upgrades an older archive during restore. Use the
backup's original source checkout to recover older data, verify recovery there,
then perform an explicit forward upgrade. Legacy format 1 manifests are refused
because they lack the required version evidence.
An existing target must have no application/public objects, extra extensions
or other sessions. It never drops a populated database. The cluster must
already contain the capability roles created by the application migrations.
For a fresh cluster, run `migrate install` to bootstrap roles in a temporary
empty application database, then have the local administrator remove that
empty database before restoring. Stop API/worker writers first. A normal
database archive preserves table owners, grants and RLS; restore also reapplies
the current environment's login credentials and database CONNECT policy.

Restore runs a single transactional `pg_restore`, compares all counts/hashes,
checks application migration checksums and the locked River history, and
compares content again. Keep a failed target
isolated for investigation; restoration does not silently delete it. Trusted
administrators must keep exclusive control of the target throughout recovery.
Archive input is copied in bounded chunks to an anonymous private spool before
being streamed to `pg_restore`. Reserve an additional archive's worth of free
space on the `work/C23a` filesystem; archive size does not drive RAM usage.
Database backups do not include LocalFS media. For a consistent database/media
cut, stop every writer, archive the media volume, record its checksum, and
recover the verified media archive alongside the database before restarting.
The rehearsal below performs this sequence.

## Rehearse recovery

```sh
bash scripts/ops_drill.sh --receipt-dir work/C23a/evidence
```

The drill generates private temporary secrets and an unpredictable project
name. It starts Compose, verifies real password logins and restricted grants,
tests all eight LOGINs over TCP4/TCP6/Unix with wrong passwords and other databases,
and refuses direct credential grants, permissive RLS policies and LOGIN-owned objects before writes.
It creates/removes its own second-database negative fixture, seeds nonempty
database, queue and media witnesses, and checks API/worker metrics. It performs
real customer registration, rejects login before proof, consumes the verification
link captured by local SMTP, logs in, and creates a customer-owned cart. It checks
the actual cookie security flags and persisted customer/tenant/cart association.
The loopback HTTP rehearsal carries those Secure cookie values explicitly;
browser/TLS cookie admission remains a separate browser gate. Proof URLs are
never fetched: their token exists only in the fragment, then a protected POST.
Tokens, cookie values, email addresses and passwords are excluded from receipts
and errors. After
quiescing API and worker it backs
up the database and media, verifies checksums, and prints `ROLLBACK_POINT`
before destroying **only its own project's volumes**. It creates a fresh
cluster, bootstraps capability roles, restores into a fresh empty database,
compares every table and media bytes, and verifies API/worker health and grants
again, and checks that the restored live session still identifies the same
verified customer and owned cart after comparing all table content hashes.
It then submits a signed synthetic inventory-expiry job and requires
River completion after restart.
Corrupted archive and populated-target probes must fail. A complete predecessor
database is built from the immutable migration prefix and backed up. Disabling
the history guards reproduces an unwanted upgrade and content mismatch; with
guards restored, the older archive is refused while the target remains absent.
`drill.json` records
the source SHA, hashes, counts, steps and explicit pending gates. On success
or ordinary failure the drill removes its project, volumes and private files.
On forced termination, use the printed project name to inspect and remove
only that project's resources; never run a global Docker prune.

Tests and drills run only in GitHub CI under the owner rule. The existing
`mvp-e2e` workflow executes this compose recovery drill. The optional
`local-ops-drill` workflow is manually dispatched because it
builds images and performs a complete volume reset. Fast preflight, backup and
restore safety tests remain in required `check_all.sh`. The manual workflow
is first dispatchable after its workflow file reaches the default branch.

Open acceptance dependency: **C-09d 合入后在演练中补验收真实下单关联**.
This card does not wire checkout's production composition root or create a SQL
order in place of the missing real flow. The receipt explicitly records the
customer order-association gate as `NOT_RUN` until that follow-up.

## Optional development quote checkout

Optional quote-order checkout uses the real Quotes/Orders/Checkout/Ledger source
composition with the neutral development Fake. Opt in with
`OPENSITE_ENVIRONMENT=test` or `development`,
`OPENSITE_CHECKOUT_FAKE_ENABLED=true`, and an independent
`OPENSITE_CHECKOUT_FAKE_SIGNING_KEY` containing 32-128 random bytes encoded as
unpadded base64url. Runtime and customer-auth database capabilities, exact HTTPS
storefront origin, and the customer CSRF secret are required. Production or
partial Fake configuration refuses startup; omitted flags keep buyer conversion
default denied. The signing key configures the same account-mapped Payments
receiver used by `OPENSITE_FAKE_PSP`; if both toggles are enabled their keys must
match. Provision an enabled Fake/test account mapping before delivering events.
Payment progresses through timestamp-signed completed Fake facts (`account_id`,
not a client-supplied tenant); the callback never issues Capture. Conversion
only creates/adopts the pending sealed order. Trusted checkout Confirm owns
capture commands and their reservation preflight. Cart checkout still needs explicit
trusted canonical rule metadata and is not enabled by this adoption capability.
The Compose API environment passes these optional variables. Direct API runs
must pass them explicitly in the process environment.

## Rotate local passwords

Stop API and worker, edit the private `.env`, then run
`migrate provision` and recreate both services. It resets approved login memberships and
passwords without printing SQL or client error payloads. Changing
`POSTGRES_PASSWORD` alone does not change an initialized PostgreSQL cluster's
administrator password: use a trusted interactive `psql` session and
`\password postgres` first, then update `.env`. Do not put passwords in argv,
SQL files or shell history. Keep the pre-rotation `.env` private until all
new pools are verified, then remove that owned copy.

## Delivery boundaries

### Configure the privacy worker and trusted JOB_TENANTS

Privacy defaults OFF. Follow [the data-rights contract](../docs/architecture/DATA_RIGHTS.md):
set `OPENSITE_PRIVACY_DATABASE_URL` for the API and `PRIVACY_WORKER_DATABASE_URL`
for the worker to the HBA-fenced `opensite_runtime_login` on the application
database, using its independently provisioned runtime credential. The worker's
queue pool still uses `opensite_queue_login`; queue credentials cannot read PII.
No new LOGIN, group or grant is needed. In `.env`, use single-quoted libpq DSNs
and keep credentials out of shell arguments, logs and version control.

Set an independent CSPRNG 32-byte hexadecimal `PRIVACY_TOKEN_KEY` on both
processes, preserve their existing shared `JOB_SIGNING_KEY`, then explicitly set
`PRIVACY_EXPORT_WORKER_ENABLED=true`. Missing API binding keeps HTTP 503 and logs
the missing setting name only. Both services mount `media_data` at the same
absolute `OPENSITE_MEDIA_ROOT`; both images prepare it for UID/GID 65532. Export
subdirectories/files retain the module's private 0700/0600 permissions.

List trusted tenant UUIDs in `JOB_TENANTS` for the existing one-minute periodic
expiry/physical-export cleanup worker. Signed export jobs use the dedicated
`privacy_exports` queue. The E2E harness generates ephemeral token/connection
configuration and verifies a scoped staff API request, worker completion and
shared-volume download. The ops drill checks default 503, explicit opt-in and a
signed sweep tick, then restores a ready export from the same media archive and
downloads it through the API before requesting a fresh export. Preserve token
and job keys together with the private volume across real recovery; follow the
data-rights erasure-journal replay procedure before serving restored data.

Outbox sending is disabled (`OUTBOX_ENABLED=false`): this local stack has no
external Sender. The actual dispatcher remains fail-closed when sending is
requested without a configured Sender; its existing integration tests exercise
a test Sender. Aggregate backlog/DLQ monitoring is live independently of
sending. `JOB_TENANTS` is a trusted comma-separated UUID configuration for
periodic expiry; empty disables periodic producers while queued signed jobs
are still processed. Keep `JOB_SIGNING_KEY` stable across worker restarts.
`OPENSITE_SERVING_REFRESH_TENANTS` is the trusted comma-separated tenant UUID list
for the API's storefront serving refresher. It rebuilds served SKU prices and
availability after price, stock and catalog changes, at least every 15 minutes, and
backfills sites published before C-15b. List every tenant with a published
storefront. An unlisted storefront falls back to "no purchasable SKUs" within
30 s of a change or 15 min of publishing. The API logs a warning at startup when
the runtime database is configured and the list is empty.
Outside this Compose stack, an existing queue-only worker configuration remains
supported with outbox disabled or unset: `/healthz` stays available, while
`/metrics` returns 503 until an outbox metrics capability is configured. Supplied
invalid or partial outbox credentials are refused instead of silently ignored.
Astro SSR is absent from this base, so storefront is omitted (`NOT_RUN`). This
is a local recovery rehearsal, with no cloud resources, TLS or real alerts.
