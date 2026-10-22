# Checkout HTTP composition (C-18a)

Bootstrap mounts these generated routes only when `Config.Checkout` is supplied.
Default configuration has no checkout transport. Explicit checkout composition
without a storefront resolver returns 401 before the service runs. Checkout uses
the tenant-bound `Storefront` RouteSpec access class with no employee scope.

Start, Preview and Get share exact HTTPS Origin and CSRF header admission,
actor/URL agreement and the resolver's private-field `AuthorizedActor`.
The concrete guest resolver verifies a canonical 32-byte capability, its
tenant/origin-bound proof and current cart ownership before issuing an actor.
Bootstrap selects a configured live customer session when its cookie is present;
an invalid cookie cannot fall back to guest authority, including when customer
authentication is unconfigured. Customer writes retain signed session CSRF.
It never authenticates an admin cookie or infers authority from Host/URL/body.
Staff actor, response and transaction-authority extensions forward on staff routes.
Preview and Get use read-only repeatable-read snapshots; Start retains canonical
rules, cart version checks and holder-bearing Orders/Inventory transitions.
Guest contact is a snapshot;
customer identity comes only from the actor and customer contact is omitted.

Runtime cart checkout is opt-in through `OPENSITE_STOREFRONT_KEY`, tenant/origin
and trusted `OPENSITE_CHECKOUT_RULE_METADATA`, with the isolated Fake Payments
receiver already configured. It preserves that receiver and uses authorized
transactions; only capability verification uses a read-only pre-admission snapshot.

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

All routes reuse strict generated OpenAPI decoding, bounded write-body admission and
RFC 9457 errors. Full route-contract reconciliation uses actual composed
handlers; the default unconfigured composition fails the full implemented
contract, and its separate witness excludes these four optional routes.
