# ADR-070 — Use pgx/v5 + sqlc; no core ORM

**Status:** Accepted as engineering baseline, subject to implementation validation.
**Date:** 2026-10-11

## Context

OpenSite Commerce requires composite tenant foreign keys, PostgreSQL FORCE RLS,
forward-only migrations, explicit publication transactions, SKU import with COPY,
price/inventory correctness and reusable API contracts for agents and browsers.

## Decision

1. PostgreSQL is the authoritative data model. Schema evolution is explicit SQL,
   applied as forward-only, checksum-protected migrations using a dedicated role.
2. `pgx/v5` is the target database driver and connection pool.
3. `sqlc` produces typed Go accessors from checked-in, reviewed SQL.
4. All tenant-scoped reads/writes run inside a transaction with a trusted,
   transaction-local `app.tenant_id`, including read-only operations.
5. Domain commands own transactions, invariants, idempotency and audit; callers
   (admin, API key, agent) cannot use a generic SQL endpoint.
6. Large SKU import uses a staging table, `pgx.CopyFrom`, validation and bounded
   batch upserts; COPY is not itself an idempotency or constraint solution.
7. Dynamic filtering uses a whitelist of sortable/filterable attributes and
   parameterized query construction; never splice arbitrary client SQL.
8. Bun JavaScript runtime remains a possible TypeScript toolchain; it is unrelated
   to Uptrace Bun Go ORM and does not replace the database layer.

## Alternatives

| Option | Strength | Why not default |
|---|---|---|
| GORM | Fast CRUD, broad associations, transactions | AutoMigrate is not a forward-only schema strategy; extra query indirection for tenant joins |
| Uptrace Bun ORM | SQL-first, relation helpers, transactions | A second query layer and database/sql pool are not justified initially |
| pgx only | Full control, COPY | Too much repetitive row scanning for routine queries |
| pgx + sqlc | Explicit SQL plus typed generated code | Requires maintaining SQL, acceptable for this domain |

No blanket performance claim is made: the ORM may be competitive on many workloads.
Business invariants and operational predictability are the deciding factors.

## Reconsider when

After three production-grade modules exist, measure repetitive CRUD effort,
complex filter query maintainability, CI time and traceability. Prototype Bun ORM
in an isolated module only if measured maintenance savings outweigh the extra
connection/transaction/permission layer. Do not introduce two write paths for the
same domain tables without a review of tenant isolation and transactions.

## Source references

- https://docs.sqlc.dev/en/latest/howto/generate.html
- https://pkg.go.dev/github.com/jackc/pgx/v5
- https://bun.uptrace.dev/guide/transactions.html
- https://gorm.io/docs/migration.html
- https://www.postgresql.org/docs/18/ddl-rowsecurity.html
