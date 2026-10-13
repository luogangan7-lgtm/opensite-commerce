# HTTP boundary (C-03ac)

`bootstrap.New` statically registers every `RouteSpec` before constructing the
server. Callers must propagate registration errors. No domain handler, database
pool or real authentication is installed by this card; `/healthz` is operational.

Private handlers run only after `Authenticator` succeeds and returns a non-nil
context derived from its input. A missing authenticator uses `DenyAuthenticator`
and returns 401. The PR-02 implementation must enforce Access, active identity,
tenant membership and scope; `AuthenticatedSelf` is reserved for the caller's
identity/session operations and cannot carry a scope or tenant parameter.
Binding that purpose to concrete OpenAPI operation declarations is deferred to
C-03b; this skeleton does not claim to validate an arbitrary handler's meaning.
Request IDs are server-generated; logs contain only ID, method, registered route
pattern, status and duration, never request bodies, credentials or query values.

`ActorResolver` is an optional extension of the configured `Authenticator`.
After private authentication and tenant/scope admission succeed, the router asks
it for `Actor{Kind, ID}` and validates a kind of `user`, `api_key` or `service`
plus a canonical, nonzero UUID. Only the router writes the private stamp read by
`AuthorizedActor(ctx)`. Public requests and returned authentication contexts
clear old actor/tenant stamps. Headers, route IDs and ordinary context values
cannot supply an actor. An authenticator without the extension remains valid
for existing actor-optional transports; commands requiring an actor return 401.
For a composed authenticator, `Actor{}, nil` means the selected underlying
authenticator has no actor projection. The empty value leaves the stamp absent
and preserves actor-optional routes. A malformed nonempty projection is rejected.

The identity PR #28 `identity.AuthorizedActor` is an alias of its privately
constructed login actor. The bridge belongs in identity or bootstrap, outside
this platform package: authenticate first, read `identity.ActorFromContext(ctx)`
from that returned context, and resolve `Actor{Kind: "user", ID: actor.UserID()}`.
Never use `actor.SessionID()` as the stable identity. A bootstrap authenticator
wrapper must forward `ResolveActor` to the selected underlying authenticator,
without inferring an identity when it is missing. Its API-key bridge must resolve
the admitted API-key ID; service identity requires a separately admitted service
ID. This projection implements no password, session, API-key, tenant-membership
or scope authentication. PR #28's current session transport only admits its
declared `AuthenticatedSelf` routes; it does not authorize tenant commands.

`DecodeJSON` accepts one DTO object with `application/json` (parameters allowed).
The default wire limit is 1 MiB, overridden by `RouteSpec.MaxBodySize`. Object and
array nesting is limited to 64, counting the root as 1; decoded keys and string
values are limited to 65,536 UTF-8 bytes. Decoded member names must be unique
within each object, including nested objects inside arrays and opaque/custom
payloads; equivalent escaped keys are duplicates. This check runs before DTO
decoding, so a duplicate-key failure leaves the destination untouched.
Size failure is 413, media failure 415,
all structural/limit failures 400. The caller writes errors with `WriteProblem`.
Ordinary struct DTOs inherit `DisallowUnknownFields`; `RawMessage` represents
intentionally opaque content and custom `UnmarshalJSON` must validate its own
fields. Discard the destination after any decode error.

Responses are buffered up to 1 MiB, so a panic replaces partial bytes/headers
with a redacted 500 problem. This skeleton does not support flushing, hijacking
or streaming. Upload/download transports belong to a separate card. Domain
transactions and effects must handle their own rollback; buffering only controls
the HTTP response. Problems have stable `code` and correlated `request_id`.

Real auth/session, OpenAPI strict generation, module registry/import rules and
operation coverage remain in PR-02 / C-03b / C-03d / C-03e respectively.

`Storefront` is a separate authenticated access class for tenant cart capabilities.
It requires the canonical tenant path and forbids staff scopes. A missing
Authenticator still denies it. Bootstrap delegates these routes only to the
cart module, which admits anonymous creation and otherwise requires a guest
capability or a trusted customer resolver. Staff cookies/API keys grant no cart
access. Cart responses use no-store and strong whole-representation ETags.

Generated server adapters belong only to this platform package. `CartHandler`
consumes `cart/contract.Service`; bootstrap mounts `carthttp.NewCartHandler(cartModule)`
without exposing generated HTTP DTOs inside the cart business module. Its table
tests live in `carthttp/cart_test.go`, alongside shared admission and router tests.
