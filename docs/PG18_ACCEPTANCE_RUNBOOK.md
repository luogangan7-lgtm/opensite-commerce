# PR-01 acceptance runbook — PostgreSQL 18 + sqlc + pgx

Status: **NOT ACCEPTED** until all blocking gates pass on a real PostgreSQL 18
instance. The offline mocks intentionally do **not** execute SQL.

## Route A — Mac/Linux with Docker

Prerequisites: Docker Engine/Desktop with Compose V2, `go` >=1.25,
`sqlc` v1.31.1, access to Go module dependencies, and a disposable database.

```bash
cp deploy/env.example .env
# Replace the placeholder with a strong local testing password.
# Never commit .env.
unset DATABASE_URL
docker compose -f deploy/compose.yaml up -d
# Confirm the postgres:18 container is healthy.
docker compose -f deploy/compose.yaml ps
./scripts/check_pg.sh
```

The SQL gate runs the **nine** numbered, checksum-locked migrations twice and
executes five SQL tests:

1. `tests/db_security_audit.sql`: FORCE RLS, policy existence, roles and grants.
2. `tests/db_smoke.sql`: minimal schema and tenant behavior.
3. `tests/db_isolation.sql`: default-deny and cross-tenant foreign key.
4. `tests/db_role_boundaries.sql`: actual `SET ROLE` reads for `app_auth` and
   `app_domain_resolver`.
5. `tests/db_release_integrity.sql`: cross-site route and published-pointer
   foreign key constraints.

`check_pg.sh` exits nonzero on the first psql failure. These tests should be
run only against an **expendable** instance. The tests use rollback-only
transactions for fixtures; migrations intentionally persist.

## Route B — accessible PostgreSQL 18 server

Set `DATABASE_URL` to a privileged **test migrator** DSN and run:

```bash
DATABASE_URL='postgresql://.../opensite?sslmode=require' ./scripts/check_pg.sh
```

`psql` must be installed. Do not expose the privileged migrator DSN to the
production app or store it in a shell script committed to Git.

## Code generation / typed adapter compilation

Use Go 1.25+ (CI uses 1.26). Existing offline source still specifies `go 1.23`
because the current authoring environment has no newer Go runtime or pgx
packages; raise this as part of the generator bootstrap:

```bash
go mod edit -go=1.25
go get github.com/jackc/pgx/v5@v5.10.0
```

Install **sqlc v1.31.1**, verify the release archive SHA-256 against the
official GitHub release. For Apple Silicon, `sqlc_1.31.1_darwin_arm64.tar.gz`
SHA-256 is:

`21602158c99eb1f2bae197a66abfb1941d1e9e50b23125bb193349c6b1acc71e`

For Linux x86-64, `sqlc_1.31.1_linux_amd64.tar.gz` SHA-256 is:

`497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354`

Then:

```bash
./scripts/check_codegen.sh
# Check Go dependency and generated code into version control:
git add go.mod go.sum internal/db/
./scripts/acceptance_pr01.sh
```

`check_codegen.sh` really runs `sqlc generate`, then `go test -tags pgx`
and `go vet -tags pgx`. It does not substitute an in-memory pgx stub.

## CI

`.github/workflows/checks.yml` provisions `postgres:18` as an isolated
GitHub Actions service and runs SQL gates. It fetches a hash-verified sqlc
release archive, obtains pgx v5.10.0, checks typed Go integration, and refuses
to merge if dependency pins / generated SQL code have not been committed.
The workflow has not been run on GitHub from this local deliverable.

## Failure classification

- `PASS`: real PostgreSQL 18 and actual `sqlc generate`/pgx compile complete.
- `FAIL`: a real SQL or Go check executed and rejected the implementation.
- `BLOCKED`: missing tool/service/network prevents a test from starting.
- `NOT RUN`: test was never executed.

Do not replace `BLOCKED` or `NOT RUN` with `PASS` based on fake psql tests.

## Known follow-up security issue

The 008 migration has wide `app_runtime` UPDATE privileges on some tenant
business tables. Before exposing production domain-verification or sensitive
media-management endpoints, narrow these grants using a **new forward-only
migration**, with separate trusted verification logic and adversarial tests.
The first accepted PR-01 is a DB foundation, not a production authentication
server or live B2C commerce API.
