# Customer authentication

Customer identities belong to one tenant and remain separate from staff users,
staff cookies, API keys and employee roles. The dedicated customer database
login has only `app_customer_auth`; startup refuses owner, runtime and staff
capabilities. New credential and session mutations use bounded proof functions.
The private executor is NOLOGIN, cannot bypass FORCE RLS and has no memberships.

Email normalization is lowercase plus leading/trailing whitespace removal.
Unicode normalization and zero-width characters remain a follow-up. A guest
customer cannot acquire credentials, a session or historical order ownership
until a single-use mailbox challenge is proved. Matching sealed guest orders
receive additive links after proof; original order contact and money snapshots
remain immutable. Existing unverified credentials retain their original PHC
after mailbox proof and can then use the reset flow.

The tenant endpoints issue an independent `__Host-osc_customer_session` cookie:
Secure, HttpOnly, SameSite=Lax, Path=/ and no Domain. Opaque session/challenge
tokens use 32 CSPRNG bytes; persistence contains only SHA256 hashes. Credentials
use Argon2id m=19456 KiB, t=2, p=1, 16-byte salt and 32-byte result. New passwords
are 12–1024 UTF-8 bytes after JSON decoding; login accepts 1–1024 UTF-8 bytes.
The limits count encoded bytes rather than characters, with no Unicode
normalization; NUL, CR and LF are rejected. OpenAPI declares these limits through
`x-opensite-min-utf8-bytes` and `x-opensite-max-utf8-bytes`, without character-count
`minLength` or `maxLength`. For example, six two-byte characters meet the new
secret minimum and 513 such characters exceed the upper bound. Unknown,
disabled, unverified and malformed-hash login paths perform the same fixed KDF
and return one credential
error. A bounded worker retains its occupied slot until computation ends.

Sessions rotate on login, expire after 30 idle minutes or 12 absolute hours by
default, and are revocable. Proof and session deadlines are computed from one
database-clock reading per issuance command; lock waiting consumes that lifetime
instead of extending it. Session liveness is checked by the database, without a
second application-clock expiry decision. Password reset consumes a short-lived token once and
revokes all sessions. Session creation compares the credential snapshot under
the customer lock, fencing a reset during password verification. Account,
IPv4/IP64 and global durable admission return 429 + Retry-After without sleeping
in request handlers. Global admission is 300/minute across all tenants; the IP
budget is 30/minute across all tenants and each tenant account is 10/minute.

Reset requests use the same preparation and mail transport path for every
syntactically valid address, including unknown, disabled and unverified accounts.
Those ineligible accounts receive an unusable challenge: no reset token is
persisted, and token exchange returns the same invalid-token response. Transport
failure therefore cannot reveal whether an account exists. This behavior remains
restricted to the permitted development recipients above.

Durable admission serializes global, IP, then tenant account budgets.
`customer_auth_global_limits` owns shared global/IP rows without `tenant_id`;
`customer_auth_limits` contains only tenant account rows under FORCE RLS. The
private NOLOGIN executor alone has minimal direct counter privileges; the
dedicated login invokes bounded functions and cannot read or write shared rows.
Windows use the database clock, counters saturate at each cap plus one, and
global/IP refusals allocate no subsequent IP/account keys. IP failure cooldown
also spans tenants; account failure cooldown remains tenant-scoped.

All replicas sharing this database must use the same configured auth secret so
canonical IP HMACs remain stable across replicas. Only the opaque canonical
IPv4 or IPv6 /64 hash is persisted, never raw IP addresses. Secret changes need
a coordinated rollout because different secrets create different IP budgets.
Retention and pruning of expired opaque admission keys need a maintenance
follow-up before enabling real public email traffic; this card does not add an
unbounded in-request cleanup operation.

GET session without a cookie issues a five-minute signed CSRF challenge. Every
write requires the exact configured Origin and signed CSRF bound to origin,
tenant and session/challenge. Cookies survive until the absolute deadline; the
database checks the idle deadline on each use. No bearer token appears in JSON.
Only a conclusively invalid bearer clears the customer cookie. Temporary store
failure returns 503 and preserves the cookie and identity. An expired or
other-tenant cookie falls back to existing guest admission without granting any
customer identity.
Verified customer context is wired into the existing cart resolver. When
checkout is configured without an explicit resolver, bootstrap resolves the
live customer session once before checkout admission, retains its public actor
context, and reuses cart's tenant and mixed guest-token checks. The automatic
checkout resolver constructs its actor from that admitted context. Guest checkout
requires both the existing cart token and an independent anonymous signed CSRF
challenge; a stale customer cookie is not proof. Live customer sessions mixed
with guest cart tokens remain rejected. Checkout never uses staff fallback and
shares the configured customer Origin;
a conflicting Origin refuses startup. Explicit checkout resolvers are preserved.
The outer wrapper also forwards staff identity projection for CMS and other
private routes using their authenticated context. Customer privacy can consume
the same public actor context; purchasing business logic is outside this card.
The purchasing BFF uses the generated customer registration, verification and
session endpoints in configured backend mode. It relays only the two customer
Host cookies with Secure, HttpOnly, SameSite=Lax and Path=/, and keeps bearer
and upstream CSRF material out of browser JSON and the cart custody cookie.
The BFF validates a presented session with the backend before projecting a
customer actor; an invalid or unavailable session fails the request without
falling back to guest purchasing. Its customer write proof uses the same
`OPENSITE_CUSTOMER_CSRF_SECRET` and pinned `OPENSITE_STOREFRONT_ORIGIN` as Go.
Identity changes discard the former cart, checkout and guest capability.
Contract fixture mode remains separately labeled and cannot establish real
authentication. The full registered-purchase/history E2E is a separate C-09f-2
acceptance unit; these transport tests do not establish that journey.

Only Fake or loopback SMTP is supported. Challenge mail refuses recipients
outside `.test`, `.invalid` and localhost, even with Fake. Links place the
one-time token in a browser fragment; a frontend must consume it by POST and
clear the fragment. Transport errors never include credentials or tokens.
Production auth configuration without a permitted Sender fails closed. This
card does not enable real email delivery. The storefront `/customer/verify`
page clears the mailbox fragment before IO and submits its token only by POST
after explicit confirmation; the backend owns tenant binding and one-use proof.

C-05c leaves three follow-up questions open:

- Direct SMTP delivery occupies an authentication request slot. Delivery remains
  restricted to local development/Fake recipients; moving it to asynchronous
  dispatch requires a separate notification follow-up before public delivery.
- Legacy unverified credentials retain their PHC until mailbox proof. There is
  currently no creation path for this legacy state; enabling one must reopen the
  potential pre-hijacking question and require proof before selecting credentials.
- The shared global admission budget remains 300/minute across all tenants.
  Capacity and tenant isolation under legitimate traffic need a follow-up before
  expanding traffic; this card does not change the shared CPU protection policy.

The CI compose drill covers registration, local mailbox proof, customer login,
customer cart ownership and backup/restore witnesses. Production checkout wiring
belongs to C-09d and is not added here: C-09d 合入后在演练中补验收真实下单关联.

All automated evidence is GitHub CI only. Local tests, builds, generators and
containers are not run. The PR contains the tests-first RED runs, GREEN SHA,
temporary fault-branch receipts and generator artifact provenance.
