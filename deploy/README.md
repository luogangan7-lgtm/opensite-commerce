# Local operations rehearsal

This stack uses Docker Compose v2, Python 3.11+ and pinned PostgreSQL 18.6 and
Go 1.27.2 images. Run commands from the repository root. It exercises local
synthetic data; cloud deployment, TLS, real credentials and alerts are outside
this stack.

## Start and migrate

Copy `.env.example` to `.env`, restrict its permissions (`chmod 600 .env`) and
replace all five password placeholders with independent random values of at
least 24 characters. Use simple `KEY=value` or single-quoted values, with no
interpolation or multiline values. Never commit the file or print resolved
Compose configuration. The helpers read `.env`; exported variables take
precedence. Set `OPENSITE_ENV_FILE` to use another private file and
`COMPOSE_PROJECT_NAME` to isolate a stack.

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --build --wait api
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/metrics
```

Postgres must become healthy before the migrator runs. The migrator checks the
cluster, applies the frozen forward-only/checksum-checked `migrate.sh`, and
provisions restricted login identities. API startup waits for successful
migration and verifies its separately configured runtime, auth, domain and
media pools. Auth/domain consumers remain pending their application wiring;
configuration does not install an authenticator. The separate media identity
uses the existing media capability rather than granting it to runtime.

Run an explicit migration/provisioning step after changing migration files:

```sh
docker compose --env-file .env -f deploy/compose.yaml run --rm migrate install
```

Only the migrator and PostgreSQL service receive the administrator password.
API logins are `NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`, with exactly
one approved group membership each. Database `CONNECT` is revoked from PUBLIC.
See [runtime-roles.md](runtime-roles.md). `/healthz` is process liveness (204),
not a continuous database-readiness probe. Compose also checks PostgreSQL
readiness. HTTP metrics count requests, errors and latency with bounded
method/status labels; they contain no path, query, customer or tenant labels.

## One application database per cluster

Before migration or restoration, preflight inspects every connectable,
non-template database as administrator. Another database containing both the
`app` schema and `public.schema_migrations` causes
`SECOND_APPLICATION_DATABASE`; failed inspection also refuses deployment.
Plain databases are permitted. V1 database names must be lowercase ASCII
identifiers (letters, numbers and underscores, maximum 63 characters).

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
deterministic content hashes for every `app` table and the migration ledger.
Dump and manifest share one exported REPEATABLE READ snapshot. Restrict access
to both files: archives contain all application data. `OPENSITE_BACKUP_DIR`
or `--directory` changes the output location. This is a full backup, without
WAL archiving, PITR or encryption-at-rest automation.

Restore targets only the configured `OPENSITE_DB`, verifies the archive hash
before touching PostgreSQL, and creates an absent database from `template0`.
An existing target must have no application/public objects, extra extensions
or other sessions. It never drops a populated database. The cluster must
already contain the capability roles created by the application migrations.
For a fresh cluster, run `migrate install` to bootstrap roles in a temporary
empty application database, then have the local administrator remove that
empty database before restoring. Stop API/worker writers first. A normal
database archive preserves table owners, grants and RLS; restore also reapplies
the current environment's login credentials and database CONNECT policy.

Restore runs a single transactional `pg_restore`, compares all counts/hashes,
checks migration checksums, and compares content again. Keep a failed target
isolated for investigation; restoration does not silently delete it. Trusted
administrators must keep exclusive control of the target throughout recovery.
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
creates/removes its own second-database negative fixture, seeds nonempty
database and media witnesses, and checks metrics. After quiescing API it backs
up the database and media, verifies checksums, and prints `ROLLBACK_POINT`
before destroying **only its own project's volumes**. It creates a fresh
cluster, bootstraps capability roles, restores into a fresh empty database,
compares every table and media bytes, and verifies API health and grants again.
Corrupted archive and populated-target probes must fail. `drill.json` records
the source SHA, hashes, counts, steps and explicit pending gates. On success
or ordinary failure the drill removes its project, volumes and private files.
On forced termination, use the printed project name to inspect and remove
only that project's resources; never run a global Docker prune.

The optional `local-ops-drill` workflow is manually dispatched because it
builds images and performs a complete volume reset. Fast preflight, backup and
restore safety tests remain in required `check_all.sh`. The manual workflow
is first dispatchable after its workflow file reaches the default branch.

## Rotate local passwords

Stop API (and a future worker), edit the private `.env`, then run
`migrate provision` and recreate API. It resets approved login memberships and
passwords without printing SQL or client error payloads. Changing
`POSTGRES_PASSWORD` alone does not change an initialized PostgreSQL cluster's
administrator password: use a trusted interactive `psql` session and
`\password postgres` first, then update `.env`. Do not put passwords in argv,
SQL files or shell history. Keep the pre-rotation `.env` private until all
new pools are verified, then remove that owned copy.

## Pending integration

PR #25 is not in this base, so `worker` is an opt-in `pending-worker` placeholder
that exits 78 with `NOT_RUN`; it does not claim job processing or monitoring.
The worker/outbox backlog and DLQ metrics snapshot interface has an explicit
skipped test until that integration. Astro SSR is also absent from this base,
so storefront is omitted. These are integration dependencies, not a complete
commerce deployment. Queue-schema backups must be added when River merges.
