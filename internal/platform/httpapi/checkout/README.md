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
`development` environment and a trusted Payments `Receiver`. Enabling it in any
other mode, or supplying only the deprecated `Verifier`, fails startup. The raw
wrapper retains exact bytes and one `X-Fake-Signature`; strict OpenAPI admission
rejects unknown, duplicate and case-variant fields before invoking the receiver.
The envelope identifies `account_id`, never `tenant_id`. Payments authenticates
`t=<timestamp>,v1=<MAC>` over timestamp + dot + raw bytes, checks freshness against
the database clock and derives authority from trusted account configuration.

The existing webhook consumes captured, failure, cancellation and refund facts;
it never invokes Checkout `Confirm` or provider Capture/Cancel/Refund commands.
A successful response is a durable `{event_id, status}` receipt, `pending` or
`processed`. Rejected receipts become a safe 409; receiver failures use the
shared redacted RFC 9457 boundary. `NewFakeHMACVerifier` remains solely as a
fail-closed retirement guard, preserving the timestamp-free replay regression.
Signature and transactional correctness belong to Payments tests; HTTP tests
prove raw handoff, strict rejection before receive and nonproduction composition.

Both routes reuse strict generated OpenAPI decoding, bounded body admission and
RFC 9457 errors. Full route-contract reconciliation uses actual composed
handlers; the default unconfigured composition fails the full implemented
contract, and its separate witness excludes only these two optional routes.
