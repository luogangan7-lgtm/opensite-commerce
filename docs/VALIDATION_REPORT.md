# OpenSite Commerce V0.7 — validation report

**Run date:** 2026-10-11  
**Environment:** Go 1.23.2, Python 3, offline Linux container.  
**Scope:** first engineering increment, not production storefront.

## Executed

| Item | Result | Evidence |
|---|---|---|
| `GOPROXY=off go test -count=1 ./...` | **PASS** | dev API, PageDocument, LocalFS unit tests |
| `GOPROXY=off go test -race -count=1 ./...` | **PASS** | all available testable Go packages |
| `go vet ./...` | **PASS** | no diagnostics |
| Valid PageDocument fixture | **PASS** | `go run ./cmd/preflight fixtures/home-valid.json` |
| Unregistered component rejected | **PASS** | invalid fixture exits unsuccessfully |
| OpenAPI YAML and local `$ref`s | **PASS (structural)** | 12 operations / 57 references resolved |
| JSON Schema syntax and fixture validation | **PASS (structural)** | 25 schemas; valid sample accepted, invalid rejected |
| Bash migration script syntax | **PASS** | `bash -n scripts/migrate.sh` |
| Real loopback HTTP request | **PASS** | `/healthz` and `/internal/preflight/page` |
| Real local media POST | **PASS** | stored bytes read back with matching SHA-256 |

## NOT RUN / NOT IMPLEMENTED

| Item | Status | Reason |
|---|---|---|
| PostgreSQL 18 migrations | **NOT RUN** | no PostgreSQL/Docker executable available |
| Database FK/RLS smoke tests | **NOT RUN** | requires live PostgreSQL 18 |
| `sqlc generate` | **NOT RUN** | sqlc CLI and pgx packages not installed |
| Full OpenAPI validator | **NOT RUN** | only structural JSON Schema checks available |
| Production authenticated Go API | **NOT IMPLEMENTED** | domain/auth integration next PR |
| Astro SSR / React Builder | **NOT IMPLEMENTED** | renderer/editor integration next PRs |
| River background processing | **NOT IMPLEMENTED** | requires PostgreSQL/Go dependencies |
| B2B inquiry persistence / notification | **NOT IMPLEMENTED** | SQL schema only |
| 10k SPU / 300k SKU benchmarks | **NOT RUN** | generator and PostgreSQL required |
| Site Release transactions / recovery | **NOT IMPLEMENTED** | schema and contract only |

## Interpretation

The tests demonstrate **working, testable primitives**, not a live independent
storefront. In particular, an API path being present in OpenAPI does not mean
that endpoint is implemented. The `cmd/dev-api` is local-only diagnostics.

Database DDL has been reviewed and scripted but remains **unverified SQL** until
run against PostgreSQL 18, followed by FK/RLS and idempotency tests.

## Next blocking checks

1. Run PostgreSQL 18 migrations and `tests/db_smoke.sql`.
2. Run sqlc generation against a pinned version and compile generated Go.
3. Implement/verify secure authentication, `app_runtime` role login and
   tenant-context transactions before exposing management endpoints.
4. Implement atomic publish and per-reference media retention checks.
5. Add the actual Astro renderer and compare preview/published HTML.
