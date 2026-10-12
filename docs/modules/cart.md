# Cart contract (C-14a)

Cart owns `carts` and `cart_lines`. A cart records intent: immutable tenant,
owner and currency; one absolute quantity per SKU; monotonically increasing
version. It stores no quoted prices, stock reservation, coupon or checkout state.
Catalog owns SKUs, pricing supplies current money quotes, inventory supplies
available quantities, and customer owns trusted customer identity/status.

The public Go contract is `internal/modules/cart/contract`; its `Service` port,
typed actor and cart view are consumed by the platform HTTP adapter. Generated
server adaptation lives in `internal/platform/httpapi/cart.go`; cart business
packages do not import generated HTTP types. Persistence stays under cart's
internal store through a consumer-defined Repository transaction contract.
Other modules cannot import cart internals or mutate its tables. Checkout must
recompute prices, validate stock and reserve inventory in its own future flow.

## HTTP authority and shape

Bootstrap mounts `httpapi.NewCartHandler(cartModule).Routes()` with exactly four
generated strict operations:

| Method | Path under `/api/v1/t/{tenant_id}` | Operation |
| --- | --- | --- |
| POST | `/carts` | `createCart` |
| GET | `/carts/{cart_id}` | `getCart` |
| PUT | `/carts/{cart_id}/lines/{sku_id}` | `setCartLine` |
| DELETE | `/carts/{cart_id}/lines/{sku_id}` | `deleteCartLine` |

Storefront is a distinct default-deny Access class and declares no staff scopes.
The cart module resolves the exact path tenant. An optional trusted customer
resolver can supply a customer actor; real customer authentication wiring is
pending. Request JSON, staff cookies and API-key bearer headers cannot supply
customer identity. This card implements no customer login or guest/customer merge.

Anonymous POST mints a cryptographically random 256-bit base64url capability,
returned once as `cart_token`. Subsequent guest requests send `X-Cart-Token`.
Only SHA-256 bytes persist; no GET or line response exposes token or hash.
Ownership and cross-tenant misses are indistinguishable 404s. Active tenant and
active customer checks apply within the data transaction. Bootstrap with no
repository returns a safe 503; it still registers the real handlers.

Create requires three uppercase currency letters. PUT requires an explicit
integer quantity in `[0, 1000000]`; zero deletes the line. DELETE is an explicit
alternative. JSON is bounded to 4096 wire bytes and admitted through the shared
strict decoder before generated decoding: unknown fields, duplicate decoded
keys, null/missing required values, fractions, overflow and trailing JSON fail.
Identifiers must be UUIDs. Errors use `application/problem+json`.

Every success returns `Cache-Control: no-store` and a strong ETag computed from
intent version plus SHA-256 of the full current view. Price and inventory hints
can change without an intent mutation, so a version-only validator is insufficient.
ETag includes the public pricing error state and omission of unavailable monetary
fields. GET returns 200 with the recoverable partial representation when a quote
expires, is archived or overflows.
PUT and DELETE require the exact current If-Match: missing is 428 and mismatch is
412. Wildcards, weak validators and lists do not authorize a write. POST creates
an absent representation and is explicitly exempt from If-Match. Changes and
version increment must be atomic after ownership/precondition admission; rejected
writes leave intent unchanged. Reads and cart writes never reserve stock.

A priced line includes `unit_amount`, `line_amount`, `price_list_id`, `priority`
and `min_quantity`, including legitimate zero money values. An unpriced or
overflowing line instead includes `pricing_error` (`price_unavailable` or
`amount_overflow`) and omits those five fields. Line minimum required fields are
`sku_id`, `quantity`, `available_quantity` and `sellable`. The cart always includes
`id`, `currency`, `version` and `lines`; when not fully priced it includes
`pricing_error` and omits `subtotal`. Missing money means unavailable, never zero.

Positive quantity changes require a fully priced projected cart; unavailable or
overflowing projected money returns 422 and rolls back. PUT zero and DELETE can
remove a bad line and return 200 even while other bad lines remain, so customers
can repair a cart sequentially using each returned ETag. GET remains readable
through quote loss. Available quantity is a hint, not a reservation.
Currency cannot change after creation. No tax, shipping, coupons, checkout,
ordering or HTTP guest merge is included in this contract.

## Gates

`tests/test_api_contract.py` and `scripts/check_contract.py` validate full OAS,
Storefront capability declarations, bounded schemas and required preconditions.
`internal/bootstrap/contract_test.go` checks actual registered RouteSpecs against
implemented OpenAPI operations in both directions. Cart transport table tests
exercise generated wrappers, strict JSON admission, owner/tenant isolation,
creation-only token output and live price/stock ETags. Cart service/store tests
must additionally verify transactional rollback, real PostgreSQL ownership/RLS,
concurrent optimistic writes and the role matrix. Source/focused evidence does
not substitute for the parent task's frozen-SHA complete gates or independent
review.
