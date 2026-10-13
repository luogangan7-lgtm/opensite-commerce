# Fulfillment

Fulfillment owns `shipments` and immutable `shipment_lines`. The public service
accepts an already authorized tenant transaction and uses generated sqlc queries.
The strict HTTP operation `createShipment` requires `fulfillment:write`,
`Idempotency-Key` and a quoted `If-Match` containing the current **order** version.
Responses expose that version and shipment-line IDs for subsequent returns.

## Stock and quantity boundary

Migration 020 remains authoritative: payment commits the entire order's inventory
reservation. Shipment allocates those already committed units and never changes
inventory. Keeping paid reservations alive instead would also require changing
expiry sweeps and would conflict with checkout's paid semantics. Paid means sold;
deducting inventory at payment continues to prevent overselling.

For every order line, shipped quantity plus canceled unshipped quantity cannot
exceed ordered quantity. A shipment requires a paid, committed order snapshot.
Shipment, return and unshipped-cancellation commands write the same order parent
before reading quantity totals; stale repeatable-read transactions must retry.
The database independently enforces the bounds and seals each shipment. A canceled
quantity remains unavailable for shipment while its refund is pending.

Command receipts bind the expected order version and complete request shape.
An identical retry returns the original result even after the order advances;
changed reuse is rejected. Foreign-tenant resources are indistinguishable from
missing resources. The database controls lifecycle advancement and retains the
refund state when fulfillment progresses after a partial refund.

`tests/db_fulfillment_returns.sql`, race cases starting at 70, strict-handler tests
and `internal/db/fulfillment_returns_integration_test.go` exercise these boundaries.
No carrier API, shipment label, exchange or multi-warehouse routing is included.
