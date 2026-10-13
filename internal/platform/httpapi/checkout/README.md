# Checkout HTTP composition (C-18a)

Bootstrap mounts these generated routes only when `Config.Checkout` is supplied.
Default configuration has no checkout transport. Explicit checkout composition
without a storefront resolver returns 401 before the service runs. Checkout uses
the tenant-bound `Storefront` RouteSpec access class with no employee scope.

A future resolver must verify the guest capability or customer session, exact
storefront/site and tenant binding, and session-bound CSRF. This increment
checks the configured exact HTTPS Origin, CSRF header admission and actor/URL
agreement, then consumes the resolver's private-field `AuthorizedActor`.
It never authenticates an admin cookie or infers authority from Host/URL/body.
The test resolver is a test substitute; real customer/guest middleware remains
integration work. Guest contact is a snapshot;
customer identity comes only from the actor and customer contact is omitted.

The Fake callback route exists only with `FakeEnabled`, explicit `test` or
`development` environment and a signature verifier. Enabling it in any other
mode fails startup. `NewFakeHMACVerifier` accepts an isolated test key of at least
32 bytes and authenticates exact raw request bytes before constructing the
provider-neutral event. This global Fake key trusts its test envelope's tenant;
it is **not** Stripe account-to-tenant mapping and must never be used for a real
provider. Checkout must independently recheck persisted tenant/order/version,
amount, currency and provider binding and durably deduplicate the event.

Both routes reuse strict generated OpenAPI decoding, bounded body admission and
RFC 9457 errors. Full route-contract reconciliation uses actual composed
handlers; the default unconfigured composition fails the full implemented
contract, and its separate witness excludes only these two optional routes.
