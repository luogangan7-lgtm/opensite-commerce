# C-02b: staff login HTTP and session admission

The [A3 decision](APP_LAYER_DECISIONS.md#a3--staff-authentication-sessions-api-keys-db-pools)
and [C-02a global identity decision](C02A_IDENTITY_AUTH_DECISION.md) remain the
source of authentication invariants. The identity module owns global users,
sessions, account/account-IP attempt counters and known devices. Its private
sqlc store writes these tables; HTTP callers use the identity contract. Neither
self-session endpoint resolves a tenant or requires any membership or scope.

The exact wire contract is [OpenAPI](../../api/openapi.yaml):

| Method and path | Operation | Admission |
| --- | --- | --- |
| GET `/api/v1/auth/login` | `getAdminLoginChallenge` | Anonymous challenge |
| POST `/api/v1/auth/login` | `createAdminSession` | Signed anonymous cookie, CSRF header, exact Origin |
| POST `/api/v1/auth/logout` | `revokeAdminSession` | Active admin session, session-bound CSRF, exact Origin |
| GET `/api/v1/auth/session` | `getAdminSession` | Active admin session |

The anonymous `__Host-osc_login_csrf` cookie lives for five minutes. Session,
challenge and known-device cookies are Secure, HttpOnly, SameSite=Lax, Path=/
without Domain. Tokens in JSON are CSRF material; opaque session/device tokens
are delivered only in cookies. Responses prevent caching. The session cookie expires at the earlier of idle
and absolute expiry; GET session refreshes it after successful admission.
Authentication uses the direct peer IP by default. `OSC_AUTH_TRUSTED_PROXIES`
optionally lists comma-separated IPv4/IPv6 network CIDRs; surrounding whitespace
is trimmed, while empty elements, trailing separators, invalid or zoned addresses,
IPv4-mapped IPv6 prefixes and prefixes with host bits fail startup. IPv6 spelling
is normalized by the standard parser; network host bits are never silently masked.
Only configure networks controlled by the deployment. A trusted direct peer enables
`X-Forwarded-For` resolution from right to left, stopping at the first untrusted
hop; an untrusted peer cannot assert a forwarded client. `Forwarded` is ignored.
Both in-memory IP admission and durable account-IP counters group IPv6 clients by
/64 (IPv4 remains per-address). Per-IP and global admission limiters are process-local;
account and account-IP failure counters are durable database state. Multiple
application instances need a deployment-level admission policy.

A login creates a new session. If an explicit old-session token is invalid, stale
or belongs to another account, the login returns HTTP 401 and clears that cookie;
the next credential submission can recover without a manual cookie clear.
Other failures do not clear the session cookie. When a valid same-user old token is supplied, the private
store conditionally revokes only the same user's active, unexpired, non-idle
session, then inserts its successor in the same transaction. Zero updated rows
roll back before insertion. Admission checks active user status, finite clocks,
absolute expiry and idle expiry using the database execution clock before
touching `last_seen_at`. Row-lock waits must not preserve an earlier application
time sample for expiry admission. Logout retains
its global session admission even after the final membership is suspended.

The additive migration is discovered by `_identity_login.sql` suffix. It keeps
legacy session identity/hash/expiry/revocation and backfills `last_seen_at` from
`created_at`, preserving session age. Only `app_auth` receives activity-update
permission; exact role matrix and real role-negative probes remain required.

Acceptance pointers: `test_identity_login_contract.py` checks four registry/OAS
operations and injected security/ownership faults; `test_check_pg_runner.py`
checks runner submission and failure propagation without executing SQL.
`check_pg.sh` executes `db_identity_login.sql` on real PG. `check_pg_races.sh`
extracts the production sqlc rotation/insert statements and uses advisory locks
plus observed `pg_stat_activity` blocking to prove one valid successor. Rotation
fixtures and witnesses use current database time, so future-date fixtures cannot
mask an execution-clock admission failure.
`check_pg_upgrade.sh` applies every filename-ordered predecessor, seeds legacy
sessions, then verifies field preservation, age backfill, permissions and the
complete latest identity/role SQL gates. Static checks alone do not establish
HTTP or PostgreSQL runtime acceptance.

Bootstrap applies a 75-second request context deadline; the HTTP server write
timeout is 90 seconds. Login applies its own five-second `HashTimeout` context
by default across admission and repository work. These are request bounds, not
throughput guarantees.

`cmd/api` installs these routes only when all three environment variables are
present: `OSC_AUTH_DATABASE_URL` (a dedicated LOGIN inheriting only `app_auth`),
`OSC_ADMIN_ORIGIN` (an exact HTTPS origin with no path), and
`OSC_AUTH_SIGNING_KEY_HEX` (32 random bytes encoded as hex). Supply credentials
through secret management; partial configuration and excessive DB privileges
fail startup. No credentials are created by this card. Missing configuration
leaves the module uninstalled and private admission denied. A nonempty proxy
setting without the three required credentials is partial configuration and also
fails startup.

Ordinary/known-device hashing defaults to two/one slots. Per-IP admission is 30
requests per minute; ordinary/reserved global buckets together default to 300.
The ordinary IP map is bounded to 4096 entries; signed device candidates have
a separate 256-entry overflow map, with shared counters for an existing IP.
Account cooldown returns an immediate generic HTTP 429 with `Retry-After: 60`;
there is no server-side cooldown sleep. Registered and nonexistent identifiers
use the same persistent account/account-IP counter path. Wrong-password HTTP 401
and cooldown HTTP 429 remain observably different policy states; neither status
claims that an account exists. A validated known device skips account cooldown
but retains IP/account-IP checks. Ordinary and reserved hash slots are held only
for actual password KDF work, never during repository IO or cooldown processing.
Database admission is released before the KDF starts. A bounded worker owns its
actual hash slot until the primitive returns: Argon2 cannot be cancelled. On
request timeout the response returns rate limited, capacity is not freed early,
and the worker cannot perform late failure-counter or session writes. Worker
growth is bounded by the configured actual KDF slots (at most 16 total). The
legacy `Config.Wait` hook is deprecated and unused.
Five failed passwords revoke the device; the existing 90-day device expiry is
enforced. **FOLLOWUP CARD d:** per-user active known-device cap and expired-row
cleanup remain deferred; this card does not implement either lifecycle policy.

These handlers are handwritten adapters to the existing strict HTTP platform;
the 03b generated-server POC is outside this repair's scope. Registry and
OpenAPI include all four implemented operations now. Target-host Argon2 timing,
real browser/proxy integration, signing-key rotation and shared multi-instance IP/global
admission are deployment acceptance work and are NOT_RUN in this card.
