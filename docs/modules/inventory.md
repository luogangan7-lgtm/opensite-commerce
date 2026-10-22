# Inventory data contract (C-11a)

Inventory owns `warehouses`, `stock_levels`, `inventory_reservations` and
`inventory_movements`. Catalog owns the referenced SKU (`product_variants`).
Orders uses opaque `owner_type/owner_id` references validated by its controlled
commands. Non-order owners additionally prove possession of a private bearer;
identifiers, movement mirrors and idempotency keys are never authorization.

The single-warehouse MVP retains `warehouse_id` in every stock key. A warehouse
is created on the first trusted receipt/correction. `UNIQUE(tenant_id)` enforces
one warehouse; lifting that constraint is a future forward migration.

## Authority and public SQL

Every caller supplies a transaction-local `app.tenant_id`. Missing, malformed,
unknown or suspended tenant contexts are rejected. All four tables use
ENABLE/FORCE RLS with the nullif tenant policy.

`app_runtime` can SELECT the four tables and execute:

- `app.inventory_reserve(sku uuid, quantity bigint, owner_type text, owner_id uuid,
  idempotency_key text, ttl_seconds integer DEFAULT 900)` **only for exact
  `owner_type='order'`**, preserving the Orders reserve contract;
- `app.inventory_reserve_non_order(sku uuid, quantity bigint, owner_type text,
  owner_id uuid, idempotency_key text, ttl_seconds integer, bearer bytea)`;
- `app.inventory_release_non_order(reservation_id uuid, owner_type text,
  owner_id uuid, bearer bytea)` and corresponding `inventory_commit_non_order`.

Non-order callers generate 32 bytes with a cryptographic random source (for Go,
`crypto/rand.Read`) and retain the bearer privately across retries. The database
stores only SHA-256 in `holder_hash`; returned rows and movement mirrors never
contain the plaintext. A stored hash cannot be used as a bearer. Owner type/UUID
must match exactly, and possession is verified under the reservation lock **before
terminal replay**. Null/wrong-length bearers are invalid; a wrong bearer is 42501
`INVENTORY_HOLDER_MISMATCH`. The old UUID-only and owner-only terminal signatures
are removed. The old reserve signature rejects non-order callers with 42501
`INVENTORY_HOLDER_REQUIRED`. Case variants of `order` cannot use non-order APIs.

Raw `inventory_release/commit` remain executable only by `app_orders_executor`;
Orders checks exact order owner/SKU/quantity before completion. Checkout uses
Orders; Quotes conversion uses the same contract, so neither transports a
non-order bearer. No production non-order Go caller exists at this change.

Only `app_queue_worker` can execute `inventory_expire_due(batch integer DEFAULT
100)` (1–1000). This system authority accepts no target ID or caller cutoff:
only rows due by the database wall clock can expire. The production
`ControlledInventory` already uses `QueuePool`/`InQueueTenant`. Runtime cannot
sweep other actors' holds. Private finish/reserve helpers remain unreachable.

Reserve/release/commit return the reservation row. Expiry returns the number
transitioned by this call. TTL is positive integer seconds. The tenant/key
uniqueness and canonical SHA-256 bind SKU, quantity, owner, TTL and (for
non-order holds) capability hash; retries must use the original bearer. Replays
never extend expiry or append movements. An attacker cannot replace a bearer
by replaying another holder's key. Historical order hashes remain compatible.

Legacy non-order rows have a null hash. They cannot acquire a new bearer through
owner/key replay and fail closed on public completion; only due worker cleanup
reclaims them. There is deliberately no owner-derived recovery token. Integrators
must finish existing non-order holds before rollout or wait for their original TTL.

Only trusted `app_inventory_manager` can execute `inventory_adjust(sku, signed
delta, reason, reference_id, kind DEFAULT 'adjustment')`; kind is inbound,
outbound or adjustment. It creates the default warehouse/stock if needed and
records a movement. Adjustment is not idempotent: callers must not blindly retry
an unknown commit result. A future reliability command envelope owns that policy.
Provision manager login membership outside this repository; it is not a buyer
or ordinary runtime capability.

Routines use a separate NOLOGIN/NOSUPERUSER/NOBYPASSRLS/NOINHERIT executor with
no role members and fixed `pg_catalog,app,pg_temp` search path. PUBLIC execution
is revoked. The executor has only the exact table capabilities audited by
`tests/db_role_matrix.sql`; private helpers are not executable by callers.

## Invariants and errors

Quantities are bigint; `0 <= reserved <= on_hand`. Runtime cannot write tables
directly. Reserve serializes the tenant/key, then locks stock; replay does not
lock a preexisting reservation. Terminal transitions lock reservation then
stock. Expiry processes stock keys in order and skips busy reservation rows.
Use normal READ COMMITTED commands; stricter transaction levels can require
retrying the entire transaction on concurrent-update/serialization errors.

Commit after expiry is rejected using wall clock after both locks; release and
expiry preserve physical stock. Commit decreases physical and reserved stock by
quantity. Identical terminal repeats do nothing; switching terminal states fails.
Every quantity change appends one movement with matching deltas, counters,
reservation and owner references. A trigger prohibits movement UPDATE/DELETE.

Stable SQLSTATE/message pairs include 22023 `INVENTORY_INVALID_ARGUMENT` or
`INVENTORY_IDEMPOTENCY_KEY_REUSED`, P0001 `INVENTORY_INSUFFICIENT_STOCK`, P0002
not-found, and 55000 `INVENTORY_INVALID_TRANSITION` /
`INVENTORY_RESERVATION_EXPIRED`. These are SQL contracts, not HTTP responses.

Acceptance: `tests/db_inventory.sql`, `tests/db_inventory_owner.sql`; exact ACL/role matrix; and lock-synchronized
last-unit, same-key, expiry/commit and first-initialization cases in
`scripts/check_pg_races.sh`. Gate readiness requires the exact granted advisory
lock, including its two-int namespace. The waiting commit's SQL statement parks
inside a materialized volatile barrier before evaluating its call; the driver
places expiry after both its observed transaction and statement starts and before
the current database clock. The driver verifies the actual stock holder blocks commit,
requires the sweep to skip the busy reservation, and requires 55000 after that
stock lock is released, with unchanged physical stock. This witnesses the wall
clock guard against both transaction and statement clocks without a startup deadline.
CI also exercises an explicit
startup gap and rejects idle/sleeping sessions and incorrect advisory lock keys.
The optional `inventory-race-soak` workflow repeats the actual inventory block
50 times without retries and preserves every log and its SHA-bound receipt.
Terminal-key replay is checked after release, commit and expiry.
`sqlc.yaml` consumes this DDL and the generated data types in `internal/db`;
`check_codegen.sh` verifies real sqlc/pgx generation and compilation. Tests discover migration history by glob/count; no
test depends on this branch's provisional migration number.

## C-20b acceptance and change boundary

Data authority is the reservation row plus capability hash; state remains
active → committed/released/expired. Commit checks DB wall clock after both
reservation/stock waits. Reserve locks tenant/key → stock; completion locks
reservation → stock; due cleanup claims SKIP LOCKED rows and orders stock keys.
The forward migration, exact ACLs and all existing caller fixtures change together
because an old public entry would bypass the single holder invariant.

Negative coverage includes observed owner/ID/hash replay, invalid token lengths,
wrong token after terminal success, changed token on key replay, cross tenant,
legacy holds, runtime cleanup denial and order case aliases. Concurrency gates
use advisory-lock/pg_stat_activity synchronization. The upgrade case discovers
the migration by suffix and applies all predecessors before testing legacy data.
Tests, code generation and gates run only in GitHub CI; no local execution.
