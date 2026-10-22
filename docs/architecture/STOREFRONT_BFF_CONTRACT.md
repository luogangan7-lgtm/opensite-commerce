# Storefront BFF payment contract — C-09d-B

Authority: V1_DECISIONS section IX, APP_LAYER_DECISIONS A5–A6,
[PR #90 split item 2](https://github.com/luogangan7-lgtm/opensite-commerce/pull/90),
[merged #206 review](https://github.com/luogangan7-lgtm/opensite-commerce/pull/206#issuecomment-6338442768),
[issue #210](https://github.com/luogangan7-lgtm/opensite-commerce/issues/210).
Wire authority is `api/openapi.yaml` and the Payments receiver, not this explanatory note.

## Capability and guest admission

The browser holds a signed Secure HttpOnly host cookie, never the cart token in
JavaScript, JSON, URLs or localStorage. The BFF passes the capability upstream as
`X-Cart-Token`. Backend mode uses the generated OpenAPI client for cart, preview,
Start, Get and Fake event delivery; fixture mode is explicitly synthetic.
Browser writes require the exact configured Origin and a session CSRF token
before any upstream request. Forwarded host headers cannot select a tenant.
Published serving data supplies display hints; runtime Checkout owns prices,
availability and the final total. The BFF never queries PostgreSQL.

The server-to-server guest proof is HMAC-SHA256 over
`checkout:<tenant>:<cart-token>:<configured-origin>`, encoded as base64url and
sent in `X-CSRF-Token`, alongside `Origin`, `X-Cart-ID` and `X-Cart-Token`.
**The proof is non-expiring and not cart-bound by design.** It binds the tenant,
token and configured storefront origin. Every admission checks the claimed
cart's ownership in the database before issuing the guest actor. Changing a
cart ID therefore cannot confer ownership. Cookie lifetime is separate from
this proof; it does not turn the proof into an expiring credential. Browser
CSRF and this server proof are distinct credentials.

Changelog for #206 / #210: OpenAPI `GuestCapability` is an `apiKey` in the
**`X-Cart-Token` header**, replacing its former cookie declaration. This matches
the existing Go receiver. The browser cookie is BFF custody, not an OpenAPI
credential transported directly to Go. Customer-cookie rejection and staff
admission remain the backend's responsibility; this BFF backend mode is guest-only.

## Fake event and persisted completion

`OPENSITE_FAKE_PSP=1` is accepted only with `OPENSITE_ENVIRONMENT=test` or
`development`. Fake events need a server-configured `OPENSITE_FAKE_ACCOUNT_ID`
matching an enabled fake/test account mapping in the database, and the isolated
base64url `OPENSITE_FAKE_WEBHOOK_KEY`. This card creates neither mappings nor keys.
Missing/invalid account configuration fails closed. Production does not expose
the BFF Fake action. Backend BFF mode additionally requires server-configured
API/site origins, a cookie signing key and the existing storefront proof key.

The BFF reads the persisted checkout bound to its signed session. That read
supplies order, provider reference, amount, currency and payable version; the
browser chooses only `captured`, `failed` or `canceled`. The flat envelope contains
`event_id`, `account_id`, `order_id`, `order_version`, `provider_ref`, `status`,
`amount_minor` and `currency`. It never contains `tenant_id`, and capture is
`captured`, never `authorized`. Integer lexemes remain exact through int64 max.

The signature is `t=<unix-seconds>,v1=<hex-HMAC-SHA256(timestamp + "." + raw-body)>`.
Node supplies the timestamp; Payments compares freshness with DB time, including
after acquiring its existing stream lock. No Node clock decides admission,
price, inventory, reservation expiry or payment state. Account-to-tenant mapping,
Inbox identity/content binding and business transitions stay inside Payments'
existing transaction and lock order. Provider I/O stays outside DB transactions.

Delivery uses the generated `/api/v1/testing/payments/fake/webhook` operation and
an exact-byte body serializer. It carries no cart capability or guest proof.
The response is only `{event_id,status}` with `pending`, `processed` or `rejected`.
The BFF validates shape and event identity; rejected or malformed receipts fail
closed. Neither `pending` nor `processed` proves payment completion. After a
successful receipt, the BFF reads Checkout again and returns that persisted view.
Fixture delivery follows the same receipt-then-read rule, without claiming DB
durability or authenticating synthetic events.

Checkout and completion views require both persisted `state=paid` and
`payment.status=captured`. Start retries retain their idempotency key after an
ambiguous response. Fact retries retain the event ID and exact payload while
the checkout remains pending; a new signature timestamp does not create a new
fact. Terminal persisted payment state avoids another delivery. Payments owns
durable replay deduplication and rejects same-identity/different-content facts.
When Start returns a changed amount or currency, capture stays disabled until
the user explicitly confirms the displayed updated total. Upstream error detail
is never UI copy; public problem codes are bounded and messages remain local.

## Acceptance boundary

CI runs Bun BFF backend/fixture seam tests, Playwright retry/consent/completion
tests and existing real PostgreSQL Payments signature/account/freshness/replay
witnesses. Tests-only RED must precede implementation GREEN at recorded SHAs;
two CI mutation probes demonstrate signature and receipt guard sensitivity.
The BFF fetch seam is contract evidence, not a real purchase E2E. Card C owns
real publication-to-purchase composition and any visual baseline renewal.
This card introduces no migration, scope, lock hierarchy, production deployment
or new provider command. Rollback is a source revert.
