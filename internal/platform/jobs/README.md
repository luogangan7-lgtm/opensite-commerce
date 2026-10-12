# Internal task contract

River Community v0.47.0 and riverpgxv5 are pinned in go.mod/go.sum. This version
preserves the existing pgx v5.10.0/sqlc contract; v0.48/v0.49 require pgx v5.11.

Install in this order: `scripts/migrate_river.sh` creates the `queue` schema and
applies River's separate migration history as `app_river_migrator`; the existing
`scripts/migrate.sh` applies application forward migrations; verify both histories
and the role matrix. `check_pg.sh` performs this sequence. The installer requires
a controlled administrative connection that may SET ROLE to the queue owner.
API and worker credentials must never include that owner role. The installer
also supports the existing local Docker Compose database without a host port.

River owns its upstream queue table shapes. We preserve those tables rather than
adding application tenant columns/composite keys. `queue.river_job` has ENABLE +
FORCE RLS: runtime can read/insert/update only jobs whose JSON tenant matches its
transaction-local authorized tenant. The queue owner and processing role have
explicit global policies. Other River tables are inaccessible to runtime.
Native InsertTx requires job SELECT, thirteen INSERT columns, UPDATE(kind),
sequence USAGE and the unique-state helper EXECUTE; these are checked exactly.
Runtime has no queue DDL, DELETE, state UPDATE or maintenance-table access.

Authorize a tenant, enter the existing tenant transaction runner, perform domain
writes, then call `NewEnqueuer(signer).InventoryExpireTx(ctx, tx, batch)` before
committing that same pgx.Tx. The producer reads the authorized tenant GUC, signs
the fixed kind/tenant/batch and uses native River InsertTx without a pool. Any
validation or River error explicitly rolls back, even after context cancellation.
Do not retain or asynchronously reuse the caller's transaction.

Build `cmd/worker` with `-tags pgx`. Configure WORKER_DATABASE_URL using a login
permitted to SET ROLE app_queue_worker, and JOB_SIGNING_KEY with a private random
key of at least 32 bytes, hex encoded. The producer and worker share this key;
runtime SQL access alone cannot forge a different tenant's executable job. Never
log or commit the key. The worker verifies the stored kind and HMAC before calling
only `app.inventory_expire_due` through tenanttx and generated sqlc. Unknown kinds
are absent from the registry; invalid signed contexts are canceled without work.
The worker has queue processing CRUD and the expiry capability, no app table
write/read rights or DDL. River REINDEX is disabled; other leader maintenance and
stuck-job rescue remain enabled. Queued jobs and retries are at-least-once.

JOB_TENANTS is an optional comma-separated deployment-authorized tenant list.
Each configured tenant schedules one batch every minute and once at startup.
Batch is bounded by the SQL service limit (1..1000). A zero expiry result may mean
SKIP LOCKED skipped due rows; the worker waits for the next interval and does not
loop until zero. Expiry's inventory state transition/movement is idempotent, so a
recovered or duplicate task has no repeated release side effect.

Operators must provision the restricted login membership, protect the signing
key and choose the tenant list. Multi-key rotation and tenant discovery are future
work: replacing the key invalidates pending signatures, so drain or explicitly
reissue pending tasks under a controlled rotation procedure.
