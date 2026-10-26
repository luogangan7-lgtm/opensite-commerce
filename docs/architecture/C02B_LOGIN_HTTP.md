# C-02b: staff login HTTP and session admission

The [A3 decision](APP_LAYER_DECISIONS.md#a3--staff-authentication-sessions-api-keys-db-pools)
and [C-02a global identity decision](C02A_IDENTITY_AUTH_DECISION.md) remain the
source of authentication invariants. The identity module owns global users,
sessions, account/account-IP attempt counters and known devices. Its private
sqlc store writes these tables only inside `tenanttx/pgxadapter.IdentityRunner.InMaintenance` callbacks under the dedicated auth LOGIN; no raw connection, pool, DBTX or Begin capability is exposed to the module. HTTP callers use the identity contract. Neither
self-session endpoint resolves a tenant or requires any membership or scope.

The exact wire contract is [OpenAPI](../../api/openapi.yaml):

| Method and path | Operation | Admission |
| --- | --- | --- |
| GET `/api/v1/auth/login` | `getAdminLoginChallenge` | Anonymous challenge |
| POST `/api/v1/auth/login` | `createStaffLoginSession` | Signed anonymous cookie, CSRF header, exact Origin |
| POST `/api/v1/auth/logout` | `logoutCurrentStaffSession` | Active admin session, session-bound CSRF, exact Origin |
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

A login creates a new session. An invalid, stale or other-account old-session cookie does not block a fresh
credential login. A read-only preflight before KDF preserves an observed active
old token for the transaction CAS; losing rotation returns HTTP 401 and emits no
cookies, so it cannot overwrite a concurrent winner. Password login locks the user
before sessions and rechecks the verified PHC after any user-row wait. Every
password change revokes all sessions in the writer transaction; a same-value
write preserves them. The pending H4 hardening migration owns these functions. When a valid same-user old token is supplied, the private
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

`bootstrap.FromEnvironment` (called by `cmd/api`) installs these routes on the
same auth DSN it already derives for the auth capability
(`OPENSITE_AUTH_DATABASE_URL`, or `OPENSITE_AUTH_DB_PASSWORD` for the
`opensite_auth_login` LOGIN inheriting only `app_auth`), plus
`OSC_ADMIN_ORIGIN` (an exact HTTPS origin with no path) and
`OSC_AUTH_SIGNING_KEY_HEX` (32 random bytes encoded as hex). Supply credentials
through secret management; partial configuration and excessive DB privileges
fail startup and name the missing variables. A deployment that configures the
auth capability (as Compose does) therefore cannot start without login routes.
Only when no identity setting exists at all (for example the storefront e2e
API) is the module uninstalled and private admission denied.

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

## Session admission availability (b-auth)

The existing API `/metrics` collector now reports process-local, bounded-label
`opensite_auth_operations_total{stage,outcome}` and
`opensite_auth_duration_seconds{stage}` histograms. `session_admission` counts
`admitted`/`denied` before storage; it never labels IPs, tokens or tenants.
`staff_session_queue` measures actual pool acquisition; `staff_session_pool`
includes acquisition, role validation, callback and commit. Credential operations
use `staff_credential_queue`/`staff_credential_pool`; unclassified adapter calls
use `staff_queue`/`staff_pool`. Pool outcomes are `ok`, `error`, `timeout`;
SQL lock/statement timeouts and expired caller/operation contexts count as timeout.
Metrics are scoped to the API server collector, not persistent identity state.

Initial operational warning thresholds (evaluate only while requests are present):

- Admission denial ratio above 1% over 5 minutes:
  `sum(rate(opensite_auth_operations_total{stage="session_admission",outcome="denied"}[5m])) / clamp_min(sum(rate(opensite_auth_operations_total{stage="session_admission"}[5m])), 0.001) > 0.01`.
- Session acquisition p95 above 250ms for 5 minutes:
  `histogram_quantile(0.95, sum by (le) (rate(opensite_auth_duration_seconds_bucket{stage="staff_session_queue"}[5m]))) > 0.25`.
- Session pool timeout ratio above 1% over 5 minutes:
  `sum(rate(opensite_auth_operations_total{stage="staff_session_pool",outcome="timeout"}[5m])) / clamp_min(sum(rate(opensite_auth_operations_total{stage="staff_session_pool"}[5m])), 0.001) > 0.01`.

Escalate at 5% denials or timeouts for 2 minutes. These are documented alert
policies for the deployment monitor, not claims of an installed alerting service.
Before scaling, collect per-instance admitted/denied rates, queue p95/p99,
timeout ratios and representative burst arrival rates. Production traffic has
not been measured by this card; CI load is not a capacity estimate. Preserve the
600/minute global, 60/minute IP, two-inflight session limits, one session DB
connection, 500ms lock/1500ms statement/2s operation budgets until that review.
