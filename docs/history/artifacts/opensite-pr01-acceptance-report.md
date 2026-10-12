# PR-01 acceptance execution record

Date: 2026-10-11. Worktree: `opensite-commerce-pr01-acceptance`.

## Blocking gates — local results

| Gate | Result | Observation |
|---|---|---|
| PostgreSQL 18 nine migrations, twice | BLOCKED | No PostgreSQL 18 server, `psql`, Docker or Podman; `check_pg.sh` exits 4. |
| FORCE RLS and actual FK / database role tests | NOT RUN ON POSTGRESQL | Five SQL test files prepared and fed to mock psql; **SQL not executed**. |
| `sqlc generate` 1.31.1 | BLOCKED | `sqlc` binary absent; `check_codegen.sh` exits 4. |
| `pgx/v5` adapter compile | BLOCKED | Go 1.23.2, pgx module absent, DNS unavailable; real module cannot be fetched. |
| CI workflow executed on GitHub | NOT RUN | No OpenSite Commerce repository was supplied or modified. |

## Completed checks (real execution, no database)

| Test | Result |
|---|---|
| `GOPROXY=off GOSUMDB=off go test -race -count=1 ./...` | PASS |
| `GOPROXY=off GOSUMDB=off go vet ./...` | PASS |
| `./scripts/check_all.sh` | PASS (negative fixture intentionally fails and is correctly rejected) |
| OpenAPI structural checker | PASS: 12 operations, 57 local references, 25 schemas |
| sqlc config/query static checker | PASS: 40 DDL tables, 10 named queries; **not sqlc generation** |
| Migration runner fake-psql test | PASS: nine locked batches + history gate; **not SQL execution** |
| Acceptance command fake-psql test | PASS: all five SQL test files submitted; error propagation confirmed; **not SQL execution** |
| Shell syntax checks | PASS |
| Migration file byte comparison versus PR-01 source | PASS: all 001–009 unchanged |

## Fixes in this increment

1. `check_pg.sh` now runs the previously omitted `db_smoke.sql`.
2. Added `db_role_boundaries.sql` for real-role authentication/domain policy behavior.
3. Added `db_release_integrity.sql` for cross-site publication constraints.
4. `migrate.sh` requires contiguous numbered migrations, refuses out-of-order
   migrations, and fails on an applied version missing from current source,
   including **before** applying a new migration.
5. `db_security_audit.sql` checks for actual tenant policy presence, not just
   RLS/ FORCE flags.
6. Added `scripts/check_codegen.sh`, `scripts/acceptance_pr01.sh`, offline
   orchestration regression tests, pinned tool requirements and runbook.
7. GitHub workflow uses real codegen check and refuses uncommitted generated
   artifacts (`go.mod`, `go.sum`, `internal/db`).

## Explicit non-claims

No PostgreSQL 18 database was reached; no DDL was executed; no `sqlc` binary
ran; pgx typed code was not compiled. The new SQL tests are authored, not
validated against a server. This is a better **test harness**, not a database
acceptance PASS. Follow `docs/PG18_ACCEPTANCE_RUNBOOK.md` in a PostgreSQL 18
runtime and preserve its actual output before marking this gate green.
