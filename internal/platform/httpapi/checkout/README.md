# Checkout HTTP composition (C-18a / C-09d)

Bootstrap mounts these generated routes only when `Config.Checkout` is supplied.
Default configuration has no checkout transport. Explicit checkout composition
without a storefront resolver returns 401 before the service runs. Checkout uses
the tenant-bound `Storefront` RouteSpec access class with no employee scope.

The configured guest resolver verifies an exact HTTPS storefront origin,
server HMAC proof, tenant, cart ID and the cart's stored token hash. Tokens are
32 random bytes encoded as base64url; only SHA-256 hashes reach the database.
Hash/proof comparisons use constant-time comparison. Missing or invalid
capabilities return 404. The Astro BFF keeps the capability in a signed Secure
HttpOnly host cookie, checks browser Origin and session-bound CSRF, and supplies
the server proof. Tokens never enter query strings or browser storage.
Neither staff authentication nor customer login is part of this composition.

The Fake callback route exists only with `FakeEnabled`, explicit `test` or
`development` environment and a signature verifier. Enabling it in any other
mode fails startup. `NewFakeHMACVerifier` accepts an isolated test key of at least
32 bytes and authenticates exact raw request bytes before constructing the
provider-neutral event. This global Fake key trusts its test envelope's tenant;
it is **not** Stripe account-to-tenant mapping and must never be used for a real
provider. Checkout must independently recheck persisted tenant/order/version,
amount, currency and provider binding and durably deduplicate the event.

All four routes reuse strict generated OpenAPI decoding, bounded body admission and
RFC 9457 errors. Full route-contract reconciliation uses actual composed
handlers; the default unconfigured composition fails the full implemented
contract, and its separate witness excludes these four optional routes.

`FromEnvironment` composes the real Cart, Rules, Orders, Ledger and Fake Payment
ports only with `OPENSITE_FAKE_PSP=1` and `OPENSITE_ENVIRONMENT=test|development`.
Any other environment with that flag fails startup; an absent/zero flag leaves
the Fake route absent. It also requires a runtime DSN, exact
`OPENSITE_STOREFRONT_ORIGIN`, `OPENSITE_STOREFRONT_TENANT_ID`, independent
base64url keys of at least 32 bytes in `OPENSITE_STOREFRONT_KEY` and
`OPENSITE_FAKE_WEBHOOK_KEY`, and canonical `OPENSITE_CHECKOUT_RULE_METADATA`.
Existing staff login and CMS configuration remain independent.

The server-side storefront additionally needs `OSC_COMMERCE_MODE=backend`,
`OSC_COMMERCE_API_ORIGIN`, `OSC_COMMERCE_SITE`, and an independent base64url
`OSC_COMMERCE_COOKIE_KEY` (at least 32 bytes), alongside the existing explicit
host allowlist. It shares the proof/Fake configuration with the API; none of
these keys are bundled into browser code. The site must be HTTPS except for
the isolated GitHub Actions loopback harness. Without the explicit test
provider, the checkout page remains unavailable. Account forms remain fixture-only.

Preview and completion use read-only repeatable-read transactions. Preview
reprices the live cart with server tax/shipping rules, returning decimal-string
minor units without orders, reservations or writes. Completion verifies the
same actor and cart against the durable checkout result. The CI harness
compares database snapshots around preview and verifies paid orders, committed
inventory and balanced ledger entries after the browser journey. A changed
server total requires explicit confirmation before Fake capture; ambiguous
submission retries preserve the original idempotency key.
