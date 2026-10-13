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
are 12–1024 bytes; login accepts 1–1024 bytes. Unknown, disabled, unverified and
malformed-hash login paths perform the same fixed KDF and return one credential
error. A bounded worker retains its occupied slot until computation ends.

Sessions rotate on login, expire after 30 idle minutes or 12 absolute hours by
default, and are revocable. Password reset consumes a short-lived token once and
revokes all sessions. Session creation compares the credential snapshot under
the customer lock, fencing a reset during password verification. Account,
IPv4/IP64 and tenant-global durable admission return 429 + Retry-After without
sleeping in request handlers.

GET session without a cookie issues a five-minute signed CSRF challenge. Every
write requires the exact configured Origin and signed CSRF bound to origin,
tenant and session/challenge. Cookies survive until the absolute deadline; the
database checks the idle deadline on each use. No bearer token appears in JSON.
Verified customer context is wired into the existing cart resolver. Checkout
and customer privacy can consume the same public actor context when their
storefront HTTP adapters merge; their business logic is outside this card.

Only Fake or loopback SMTP is supported. Challenge mail refuses recipients
outside `.test`, `.invalid` and localhost, even with Fake. Links place the
one-time token in a browser fragment; a frontend must consume it by POST and
clear the fragment. Transport errors never include credentials or tokens.
Production auth configuration without a permitted Sender fails closed. This
card does not enable real email delivery or add storefront verification pages.

All automated evidence is GitHub CI only. Local tests, builds, generators and
containers are not run. The PR contains the tests-first RED runs, GREEN SHA,
temporary fault-branch receipts and generator artifact provenance.
