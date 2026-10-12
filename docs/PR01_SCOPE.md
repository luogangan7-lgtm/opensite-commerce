# PR-01 — Database foundation, hardening, and typed query boundary

## What is actually implemented here

- Preserved **all eight** V0.7 migration files byte-for-byte. The new
  `009_auth_domain_roles.sql` is forward-only and adds distinct database
  roles for authenticated credential lookup and verified public domain lookup.
- Reworked migration runner: PostgreSQL 18 version check, per-migration
  transaction-level advisory lock, checksum verification *under that lock*,
  transactional migration journal, repeatable second run and drift rejection.
- Added privilege audit and cross-tenant/RLS smoke tests for PostgreSQL CI.
- Added driver-independent, testable Go tenant transaction lifecycle with
  cleanup on failure/cancellation and a PGX v5 adapter behind build tag `pgx`.
- Added Go/SQL sample typed queries for organization, site and SKU reads.
- Added restricted production role provisioning documentation.
- Added ONE CI job for PostgreSQL 18, sqlc v1.31.1, pgx v5.10.0, Go tests,
  schema checks, and source generation.

## What is NOT proven in this authoring environment

PostgreSQL 18 cannot be launched, sqlc cannot be installed, and the pgx module
cannot be fetched because neither PostgreSQL/Docker nor outbound dependency
network access exists here. Consequently **no migration has been executed**, no
sqlc code has been generated, and the pgx-tagged adapter has not been
compiled. Those are CI-only gates: **NOT RUN**, not PASS.

## Ordered production implementation after these checks pass

1. Pin pgx dependency checksums in committed `go.sum`, check in sqlc-generated
   code, and switch project minimum Go version to a currently supported one.
2. Implement authorization/membership lookup using `app_auth` with genuine
   hashed secrets, Argon2id and protected sessions; do NOT use this module as
   a production auth server yet.
3. Implement organization/site/product APIs using sqlc inside the scoped pgx
   transaction. No controller may call the privileged migration connection.
4. Connect LocalFS to media SQL states and verified asset publishing.
5. Implement Astro render context + Site Release end-to-end.

## Known architectural limitations

- Existing V0.7 schema has not been exercised on a live PostgreSQL 18 server.
- RLS is not secure against arbitrary SQL execution by the app identity.
- The schema does not enforce completeness of product SKU option sets;
  the domain layer must do so, with integration tests before catalog API.
- API contracts are structural drafts, not a deployed production API.
- Existing migration 008 grants some broad app_runtime table operations; PR-02
  must enforce authorization at every command endpoint and consider reducing
  those database grants with a new forward-only migration.
- `sqlc.yaml` currently points to 001..007; role/DO statements aren't needed
  for code generation, and CI must confirm the generator's DDL parsing.
