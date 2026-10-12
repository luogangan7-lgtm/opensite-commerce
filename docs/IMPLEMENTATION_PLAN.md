> **PR-01 update:** The live migration and role checks from item 1 are now
> defined in `scripts/check_pg.sh`, and the transaction lifecycle is implemented
> in `internal/tenanttx`. See `docs/PR01_SCOPE.md` for what remains unverified.

# Implementation plan — first vertical slice

## Completed in this increment

- Drafted eight forward-only PostgreSQL 18 migrations including identity,
  tenant/site, SPU/SKU/options/attributes, media, CMS/blog/inquiry,
  immutable site releases, mutable drafts, idempotency and FORCE RLS.
- Added SQL query examples and `sqlc.yaml` targeting pgx/v5. sqlc generation
  is **not yet run** and its compatibility is not claimed.
- Defined 12 OpenAPI 3.1 API operations with 25 JSON Schemas and bounded,
  typed component command variants. The API is a **contract**, not a server.
- Implemented standard-library Go canonical PageDocument validation and
  content-addressed LocalFS implementation, with unit tests.
- Added a loopback-only Go diagnostics server and CLI for development tests.
- Added a PostgreSQL migration/checksum script, test database Compose file,
  and SQL smoke-test fixture. PG/Docker are unavailable in the current
  working environment; tests are staged but **NOT RUN**.

## Next implementation PRs (priority order)

1. **PR-01: verify migrations against PostgreSQL 18**. Run compose and
   `scripts/migrate.sh`; fix syntax and `sqlc generate`; build migration
   role and separate `app_runtime`/auth login roles. Add no-tenant-deny tests.
2. **PR-02: authentication**. Implement password hashing (Argon2id or vetted
   equivalent), secure cookie sessions, hashed API keys, scope intersection,
   CSRF protection and rate limits. No production write endpoint before this.
3. **PR-03: Go service**. Implement org/site/catalog endpoints using pgx pool,
   sqlc, transaction-local tenant context and authoritative domain commands.
4. **PR-04: Media integration**. Connect the tested LocalFS primitive to SQL
   `media_uploads`, SHA-256/MIME validation, READY transitions, cleanup and
   authenticated streaming read routes.
5. **PR-05: Release**. Persist drafts, validate real referenced assets,
   immutable revisions, atomic publish pointer switch and rollback-by-new-release.
6. **PR-06: Astro integration**. Shared component registry, trusted internal
   Read Context API, site/route resolution, actual SSR preview and published HTML.
7. **PR-07: React admin + inquiries**. Publish B2B demo, add inquiry storage
   and async notifications. Agents must use the exact same domain operations.

## Explicitly NOT implemented

Production authentication, the real pgx runtime service, River integration,
PostgreSQL execution tests, Astro SSR, React builder, public inquiry endpoint,
admin UI, SQLC generation, image decoding/security scanning, payment/checkout,
and 300,000 SKU performance tests.

## Demo isolation rules

The `cmd/dev-api` is **loopback-only** and not production API. It never marks
uploaded bytes as `READY` in PostgreSQL and must never serve private files to
anonymous clients. Use the SQL-backed media service after PR-04.

## Architectural invariants

- Product and SKU identities are tenant scoped; composite FKs prevent
  cross-tenant links. SKU option combinations are derived by the domain layer.
- Product records are not tied to page documents or static HTML.
- `media_asset_id` is stable across storage backend migrations.
- Immutable page revisions and site release manifests are not changed in place.
- One immutable release per atomic website publish. Restore creates a new release.
- Preview and published pages will share the same Astro renderer.
- No automatic publication of unverified product facts or private documents.
