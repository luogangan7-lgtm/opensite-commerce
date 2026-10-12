# OpenSite Commerce — PR-01 database foundation (from V0.7 starter)

An **incomplete, verifiable engineering starter**, not a production storefront.
An open-source, self-hosted, API-first B2B/B2C commerce + CMS + page-builder
architecture. **No mandatory paid object storage**; LocalFS is the default.

## Repository contents

- `docs/ADR-070.md`: pgx + sqlc vs GORM/Uptrace Bun ORM decision.
- `docs/ADR-071.md`: transactional migrator + separated database trust zones.
- `migrations/001_*.sql` ... `009_*.sql`: PostgreSQL 18 schema, FORCE RLS and separated auth/domain roles. Original 001–008 migration bytes are unchanged.
- `queries/catalog.sql`, `queries/site.sql`, `sqlc.yaml`: typed SQL generation inputs.
- `internal/tenanttx/`: transaction-local tenant scope (tested with fake driver).
- `internal/tenanttx/pgxadapter/`: actual pgx adapter, compiled with `-tags pgx` in CI.
- `api/openapi.yaml`: 12-operation API contract (not fully implemented).
- `internal/pagegraph/`: schema-limited PageDocument validator.
- `internal/storage/local/`: content-addressed local file storage.
- `cmd/preflight/`: validator CLI.
- `cmd/dev-api/`: loopback-only diagnostic service; **NOT production API**.
- `deploy/compose.yaml`: PostgreSQL for integration testing, **not a full stack**.
- `tests/db_smoke.sql`, `tests/db_isolation.sql`, `tests/db_security_audit.sql`: PostgreSQL security tests (NOT RUN locally).

## Run offline core tests (Go 1.23+; pgx v5.10.0 build requires Go 1.25+)

```bash
go test ./...
go run ./cmd/preflight fixtures/home-valid.json
# Must fail:
go run ./cmd/preflight fixtures/home-invalid.json
```

## Optional dev-only HTTP diagnostics

```bash
OPENSITE_DEV_MEDIA_ROOT=.dev-media go run ./cmd/dev-api
# Bind address is always 127.0.0.1:8081.
curl -X POST -H 'Content-Type: application/json' \
  --data-binary @fixtures/home-valid.json \
  http://127.0.0.1:8081/internal/preflight/page
```

This service is intentionally **not authenticated** because it binds to loopback
and has no database credentials. **Do not reverse-proxy it or expose it publicly.**
It is strictly a primitive exercise for the validator and LocalFS storage.

## PR-01 blocking acceptance gates (PostgreSQL 18 required)

This repository is **not yet accepted**. Run the following in an environment
with PostgreSQL 18 (`psql` or Docker), Go >=1.25, sqlc v1.31.1 and the
pinned pgx v5.10.0 dependency:

```bash
./scripts/acceptance_pr01.sh
```

The script intentionally fails if a tool or database is unavailable. It runs
nine immutable migration files **twice**, then five SQL behavior tests:
`db_security_audit`, `db_smoke`, `db_isolation`, `db_role_boundaries`,
`db_release_integrity`. It then executes actual `sqlc generate`, Go tests and
`go vet` under the `pgx` build tag. It does **not** claim to have executed
against PostgreSQL until a PostgreSQL 18 server is reachable.

The migration runner rejects checksum drift, out-of-order application,
missing migration numbers and applied database migrations whose source files
have disappeared. `tests/test_check_pg_runner.py` tests invocation and failure
propagation with a fake psql; **that test never executes SQL**.

A local dependency bootstrap on a machine with network access and Go >=1.25:

```bash
go mod edit -go=1.25
go get github.com/jackc/pgx/v5@v5.10.0
# Install sqlc v1.31.1 from its official release; verify its SHA-256.
./scripts/check_codegen.sh
# Commit go.mod, go.sum and internal/db before merging PR-01.
```

## PostgreSQL 18 integration tests (Docker or existing PostgreSQL 18)

```bash
cp deploy/env.example .env
# Edit .env: choose a long random POSTGRES_PASSWORD.
docker compose -f deploy/compose.yaml up -d
./scripts/check_pg.sh
```

The schema requires PostgreSQL **18** for `uuidv7()`. The Docker named volume
mounts `/var/lib/postgresql` as recommended for postgres:18. The migration
script verifies file SHA-256 checksums and refuses modified applied migrations.
Migration access runs with a privileged role; the future `app_runtime` role
must inherit only narrowly granted permissions, with no DDL or BYPASSRLS.

## OpenAPI contract checks

```bash
python -m pip install pyyaml jsonschema
python scripts/check_contract.py
```

The checker parses YAML, resolves local `$ref`s, checks JSON Schema objects
and validates the demo PageDocument fixture. It is **not** a full OpenAPI
standards validator or proof of deployed API behavior.

## Code generation (requires installing sqlc)

```bash
./scripts/check_codegen.sh
# Pin sqlc v1.31.1 and pgx v5.10.0 for PostgreSQL-enabled CI.
```

CI pins sqlc **v1.31.1** and pgx **v5.10.0**. Generation and
compilation of the pgx adapter have **not** been tested in this offline
authoring environment. The default untagged Go packages use only stdlib.
Generated files and dependency checksums must be committed after the first
successful CI generation, before production use.

## Safety / scope

No user or agent has direct SQL access via API. The production API, auth
service, marketplace payments, inventory, SSR and admin UI remain future PRs.
Do not assume PostgreSQL migrations have passed integration tests yet.
See `docs/PR01_SCOPE.md`, `docs/PR01_VALIDATION.md`,
`docs/IMPLEMENTATION_PLAN.md` and `docs/VALIDATION_REPORT.md`.

License: Apache-2.0, see `LICENSE`.

## PR-01 changes and next gate

See `docs/PR01_SCOPE.md` and `deploy/runtime-roles.md`. The migration script
now verifies checksums **inside a PostgreSQL advisory-locked transaction**.
To test against an existing PostgreSQL 18 server without Docker:

```bash
export DATABASE_URL="postgres://<migrator>@localhost:5432/opensite?sslmode=require"
./scripts/check_pg.sh
```

CI compiles the pgx adapter with `-tags pgx` after downloading the pinned
sqlc binary and pgx library. This authoring environment cannot fetch these
dependencies or start PostgreSQL, so such checks remain NOT RUN locally.
