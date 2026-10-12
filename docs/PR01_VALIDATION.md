# PR-01 Execution Report — 2026-10-11

**Source:** incremental copy of V0.7 starter; original migrations 001–008
are byte-identical (SHA-256 comparison PASS). No existing GitHub repository was
modified, and no PostgreSQL server was provisioned.

## Checks actually executed

| Check | Result |
|---|---|
| `GOPROXY=off GOSUMDB=off go test -race -count=1 ./...` | PASS |
| `GOPROXY=off GOSUMDB=off go vet ./...` | PASS |
| `python3 scripts/check_contract.py` | PASS (structural only) |
| `python3 tests/test_migration_runner.py` | PASS (fake psql, SQL not executed) |
| `bash -n scripts/*.sh` | PASS |
| Original migration file SHA-256 equality (001–008) | PASS |
| Migration tool's no-DB failure exit code | PASS (exits 4 as specified) |

New tenant transaction tests cover: begin/scope/domain/commit error paths,
rollback on noncommitted paths (including panic), no transaction started for
invalid tenant ID, and cancellation before beginning a transaction.

## Blocked or unimplemented — NOT PASS

| Check | Result | Why |
|---|---|---|
| PostgreSQL 18 DDL/migration execution | NOT RUN | No PostgreSQL or Docker installed |
| Database RLS/cross-tenant/role audit | NOT RUN | Needs real PostgreSQL 18 |
| `sqlc generate` | NOT RUN | No local sqlc binary; no internet DNS |
| `go test -tags pgx ./...` | NOT RUN | pgx module cannot be downloaded |
| Go SQL repository + real HTTP APIs | NOT IMPLEMENTED | Requires verified pgx/sqlc and production auth |
| Admin UI/Astro SSR/publish workflow | NOT IMPLEMENTED | Later vertical slices |
| 10k SPU / 300k SKU benchmark | NOT RUN | Requires PostgreSQL |

The `.github/workflows/checks.yml` CI job includes PostgreSQL 18, a pinned
sqlc v1.31.1 release archive with published SHA-256, and pgx v5.10.0. It has
**not yet run on GitHub** in this session. A CI definition is not CI evidence.

## Security scope

The pgx adapter is tagged `pgx`, and cannot be considered compiled until a
PostgreSQL-enabled environment fetches dependencies and executes the CI job.
The driver-neutral tenanttx runner **is** unit-tested. It establishes
transaction-local tenant context but does **not** authenticate the tenant ID.
Never expose it behind a public API without real auth/membership checks.

`migrations/009_auth_domain_roles.sql` adds `app_auth` and
`app_domain_resolver` NOLOGIN roles. Login accounts/secrets must be created
out of band. The authentication server itself is not implemented.

## Next acceptance gate (must be real)

1. Provision an expendable PostgreSQL 18 database and execute
   `DATABASE_URL=... ./scripts/check_pg.sh`; verify 9 migration checksums and
   RLS/foreign-key tests with observed output.
2. On Go >=1.25 run pinned sqlc v1.31.1, generate `internal/db`, pin pgx
   v5.10.0 in `go.mod`/`go.sum`, and run `go test -tags pgx ./...`.
3. Only then implement authenticated Tenant/Organization/Site/Catalog endpoints
   using the generated queries and scoped transaction runner.
