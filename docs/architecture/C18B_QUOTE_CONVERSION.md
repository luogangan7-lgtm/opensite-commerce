# C-18b quote conversion HTTP boundary

`internal/platform/httpapi/quoteorders` adapts the quotes conversion application
through `Convert(ctx, quoteID, key)` and `GetConversion(ctx, quoteID)`. It owns no
tables, SQL, tenant transactions or provider calls. Generated server types remain
behind neutral aliases in `httpapi/contract_types.go`.

| Route | Authority | Request | Response |
| --- | --- | --- | --- |
| `POST /api/v1/t/{tenant_id}/quotes/{quote_id}/orders` | `BuyerCompany` with `quotes:convert`; router's injected buyer authenticator yields a trusted customer actor | One closed JSON object `{}`, one visible ASCII `Idempotency-Key` (1–128 bytes), Origin and session-bound CSRF | `201` conversion receipt, including same-key replay |
| `GET /api/v1/t/{tenant_id}/quotes/{quote_id}/order-conversion` | Staff authentication with `AdminOrAPIKey` and `quotes:read` | Canonical nonzero tenant/quote UUIDs | `200` conversion receipt |

The buyer route uses the explicit `BuyerCompany` access class with canonical tenant
prefix and `quotes:convert` scope. The router dispatches only to the injected buyer
authenticator, once; staff authentication is never a fallback. The adapter requires
the sealed quotes customer stamp; a nil resolver, nil context, missing stamp or
staff stamp returns 401 before any domain call. Buyer authentication must verify
the separate session, tenant, exact storefront origin, session-bound HMAC CSRF and
current company membership. The domain independently enforces current membership
and company ownership. Client headers and body fields cannot supply these facts.
The production nil resolver remains denied; this transport does not implement a
customer login system. `CustomerSession` reserves `__Host-osc_customer_session`,
matching the earlier checkout reserved name recalled from Humaux; its recognizing
middleware and login are not implemented here. `BuyerOrigin` is separate because
the existing required `Origin` parameter explicitly describes the admin origin;
shared `CSRFToken` remains appropriate for a session-bound HMAC. No staff contract
is changed. Structural absence/ambiguity of either customer protection header is
400; their authenticity is the buyer resolver's responsibility.

Authenticated tenant mismatch returns 404 before the service. The domain's company
mismatch is also mapped to 404. Invalid canonical UUIDs or keys return 400. Shared
strict JSON admission rejects unknown money/price/identity fields, duplicate keys,
null, arrays and trailing data before generated DTO decoding. The body limit is
1 KiB; unsupported media returns 415 and excessive bodies return 413. Domain state
conflict maps to 409 and idempotency key reuse maps to 422.

The public receipt contains quote/order IDs, decimal-string quote version and order
number, decimal-string amount in currency minor units, currency, order state,
creation time and an optional provider reference. Customer ID, company ID and the
idempotency key remain private domain fields. Amounts never pass through floating
point. The declared response schema validates the receipt before serialization;
invalid domain output fails closed with a redacted 500. Responses use `no-store`.

GitHub CI runs the HTTP tests with the default Go race suite. Tests cover default denial, tenant/company
isolation, seller scope, strict admission, exact amounts above JavaScript's safe
integer range, 128-byte key boundary, private-field exclusion and error mapping.
CI mutation branches disable tenant isolation and round money through float64;
each must reach its corresponding assertion in RED, while the unchanged source
passes GREEN. Historical local receipts predate the owner's CI-only rule and do
not certify the final SHA. Final module, contract and full gates run in GitHub CI.

Real customer authentication, real checkout/provider actions and payment success
are **NOT_RUN** by these HTTP tests. A conversion receipt is not payment proof.

## Transaction and integration boundary

`quotes.NewConversionApplication` locks the actor's approved quote, checks its
DB-clock expiry, creates the order through `orders.NewQuoteCreator`, and binds the
immutable quote-owned receipt in the same tenant transaction. Binding compares
both header totals and every line/tax/shipping snapshot; equal totals alone are
insufficient. It emits `quote.converted` atomically. One tenant/quote, tenant/order,
and tenant/customer/idempotency key can have only one receipt. An absent receipt
must produce a normal zero-row read, so the first conversion can continue in the
same transaction. Authorized completed history can be replayed after quote expiry;
current company membership is still checked on every replay.
After an order leaves `pending_payment`, a same-key replay returns its durable
current order state without invoking the payment port. Provider references are
optional in this historical projection. Pending receipts alone may resume prepay.

The `quote.converted` source event also freezes a notification envelope in the
same transaction. Its recipient comes from the receipt's customer, and the
notification-owned hook verifies the quote/order/version tuple before producing
`notification.dispatch`. English and Traditional Chinese templates are included.
This forward extension preserves the existing notification migration; rendering
and envelope tests prove the handoff, while real customer delivery is NOT_RUN.

Negotiated Orders input has explicit quote ID/version/line provenance. The regular
price-list creator rejects this mode, and the quote creator does not invent a
price-list ID or query current prices. Only SKU identity/label is resolved live.
A freeform quote line without a catalog variant is refused: the current Orders
contract requires a fulfillable SKU. Quote approval alone cannot create a synthetic
inventory item. The order remains `pending_payment` until checkout confirms it.

Provider work is delegated *after* conversion commits through the narrow
`quotes/contract.PrepaymentPort`. No quote-adoption implementation is copied from
checkout PR #38: that branch's current public start contract accepts a cart, not
an existing negotiated order. Until its adapter is supplied, production config
refuses conversion before any database writes. The port must implement durable
intent and payment recovery, validate owned active reservations before Capture,
and commit `paid`, the balanced ledger posting and `order.paid` together. The
conversion response verifies the order state again from the actor-filtered DB
projection; a port returning `paid` cannot forge payment proof.

`internal/bootstrap/quoteorders_pgx_test.go` supplies an explicitly test-only
Fake adapter. It exercises the real inquiry/quote/send/accept/single-approval,
Orders, inventory, ledger and outbox public contracts in PostgreSQL. Its in-memory
mutex/cache and Fake calls inside the test transaction are not durable recovery
or a production payment adapter. It uses B2C account meanings: receivable, revenue,
tax payable, shipping revenue and cash. Definitive failure or expired reservations
cancel the intent/order and release stock without a journal; ambiguous failure
keeps the same pending order and reservation for retry. The public inquiry model
currently lacks a company binding command, so test provisioning explicitly binds
that association through maintenance; a complete authenticated inquiry UI is
**NOT_RUN**. Customer-session middleware, checkout quote adoption, real providers,
crash recovery and notification delivery remain **NOT_RUN**.

Acceptance includes `tests/db_quote_conversion.sql`, native synchronized PG race
gates 100–103, `TestQuoteOrdersPG*`, `TestOrdersNegotiatedSourceAndReservationPreflight`,
quote projection/replay unit tests and strict HTTP tests. The native races use
advisory locks and observed `pg_stat_activity` lock waits; gate numbers are not
client counts. The expiry test waits for a DB-clock deadline, not sleep-based
connection ordering. The upgrade gate seeds an approved quote under all preceding
migrations and proves its snapshot checksum survives this forward migration.
