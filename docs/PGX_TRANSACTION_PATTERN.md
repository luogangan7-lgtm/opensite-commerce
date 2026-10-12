# PostgreSQL transaction and tenant scope contract

This is a **design snippet**, not compiled code in this increment (pgx/sqlc
are not installed in the offline environment).

```go
// Source of tenantID: authenticated context, NEVER an arbitrary JSON field.
tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
if err != nil { return err }
defer tx.Rollback(ctx)
if _, err := tx.Exec(ctx,
    `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
    return err
}
queries := db.New(tx) // sqlc-generated queries, using this transaction only.
product, err := queries.GetProduct(ctx, db.GetProductParams{
    TenantID: tenantID,
    ID: productID,
})
if err != nil { return err }
// Validate domain invariants, enqueue River job in the SAME tx when needed.
return tx.Commit(ctx)
```

**Invariant:** No tenant-domain SQL is allowed on a naked pgx pool or on a
connection without an authenticated, transaction-local tenant context. The
schema also uses composite FKs to prevent cross-tenant references even for
privileged migration and maintenance code.

## Separate identities

- Migrator: owns DDL and initial role grants, not used by API runtime.
- Authenticator: restricted to credential validation and membership lookup.
  Full design/authentication tests are a required next PR; this role is NOT
  created or exposed yet.
- App runtime: inherits `app_runtime` grants, no bypass of FORCE RLS, no DDL,
  no DELETE/TRUNCATE. Its login identity is created by deploy provisioning.
- Maintenance: separately authorized housekeeping, never exposed to Agent.

## RLS limitation

Custom PostgreSQL settings such as `app.tenant_id` are not secrets. RLS cannot
protect against a database client that is permitted to run arbitrary SQL on
that same connection and can change the GUC itself. Do not give users or
agents arbitrary SQL endpoints; audit all queries and isolate connections.
