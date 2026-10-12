# Inventory data contract (C-11a)

Inventory owns `warehouses`, `stock_levels`, `inventory_reservations` and
`inventory_movements`. Catalog owns the referenced SKU (`product_variants`).
Orders and checkout are opaque `owner_type/owner_id` references until their
tables exist; a future migration must tighten those references.

The single-warehouse MVP retains `warehouse_id` in every stock key. A warehouse
is created on the first trusted receipt/correction. `UNIQUE(tenant_id)` enforces
one warehouse; lifting that constraint is a future forward migration.

## Authority and public SQL

Every caller supplies a transaction-local `app.tenant_id`. Missing, malformed,
unknown or suspended tenant contexts are rejected. All four tables use
ENABLE/FORCE RLS with the nullif tenant policy.

`app_runtime` can SELECT the four tables and execute:

- `app.inventory_reserve(sku uuid, quantity bigint, owner_type text, owner_id uuid,
  idempotency_key text, ttl_seconds integer DEFAULT 900)`;
- `app.inventory_release(reservation_id uuid)`;
- `app.inventory_commit(reservation_id uuid)`;
- `app.inventory_expire_due(batch integer DEFAULT 100)` (1–1000).

Reserve/release/commit return the reservation row. Expiry returns the number
transitioned by this call. TTL is positive integer seconds, not calendar months.
The tenant/key uniqueness and canonical SHA-256 bind SKU, quantity, owner and
TTL; replay returns the original reservation without extending expiry or adding
movements. Terminal replay returns its existing state rather than reserving again.

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

Acceptance: `tests/db_inventory.sql`; exact ACL/role matrix; and lock-synchronized
last-unit, same-key, expiry/commit and first-initialization cases in
`scripts/check_pg_races.sh`. The waiting-commit case witnesses a valid reservation
blocked on stock, waits for the database clock to cross its expiry, then requires
55000 with unchanged physical stock; the sweep skips the busy reservation.
Terminal-key replay is checked after release, commit and expiry.
`sqlc.yaml` consumes this DDL and the generated data types in `internal/db`;
`check_codegen.sh` verifies real sqlc/pgx generation and compilation. Tests discover migration history by glob/count; no
test depends on this branch's provisional migration number.
