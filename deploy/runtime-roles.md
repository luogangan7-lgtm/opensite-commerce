# Provision separate PostgreSQL login identities (required before production API)

The migrations create restricted **NOLOGIN** group roles, including:

Group capabilities are NOSUPERUSER/NOBYPASSRLS/NOCREATEDB/NOCREATEROLE/
NOREPLICATION/NOINHERIT with no outward memberships. Provision LOGIN
credentials outside migrations.

| Group role | Purpose | Safety boundary |
|---|---|---|
| `app_runtime` | Tenant-scoped domain queries | FORCE RLS, no credential hashes, no DDL/DELETE/TRUNCATE |
| `app_auth` | Credential/session lookup | Not exposed to public/Agent query APIs |
| `app_returns_service` | Trusted refund coordination | Dedicated private login; never runtime, renderer or public |
| `app_customer_auth` | Tenant customer proof/session functions | Dedicated login; never combined with runtime or employee auth |
| `app_domain_resolver` | Verified hostname lookup | No credential or media access |
| `app_media_service` | Controlled media lifecycle queries | Separate trusted service pool; no general runtime credential access |
| `app_domain_verifier` | Verify/retire domain mappings | Dedicated private verifier LOGIN; tenant RLS |
| `app_publisher` | Execute seal/publish commands | Private publisher LOGIN; no direct pointer DML |
| `app_publish_executor` | Own seal/publish functions | No LOGIN or members; never provision credentials |
| `app_outbox_worker` | Claim/complete/fail delivery commands | Private worker LOGIN; fenced function capability |
| `app_reliability_executor` | Own outbox/idempotency functions | No LOGIN or members; never provision credentials |
| `app_inventory_manager` | Trusted inventory adjustments and reads | Private inventory manager LOGIN; tenant-scoped commands |
| `app_inventory_executor` | Own inventory mutations | No LOGIN or members; never provision credentials |
| `app_orders_executor` | Own order snapshot and transition functions | No LOGIN or members; never provision credentials |
| `app_queue_worker` | River jobs and controlled inventory expiry | Dedicated private worker LOGIN; queue RLS |
| `app_river_migrator` | Own upstream River schema and migration history | Separate privileged migration capability; no application LOGIN |
| `app_outbox_tenant_broker` | Enumerate active Outbox tenant IDs | No LOGIN or members; IDs/status only |
| `app_credential_writer` | Own bootstrap and bounded counter cleanup; minimum user hash/key issuance, scoped buyer-role seed and cleanup columns | No LOGIN or members; never provision credentials or grant this role |
| `app_admin_bootstrap` | Execute first-owner bootstrap only | Dedicated offline bootstrap LOGIN only; no table/column privileges |

The migrator's privileged LOGIN is a separate operator identity, not an
application role. It owns schema installation/checksums and is never used by
the API or bootstrap CLI. Business roles receive their own private LOGINs; do
not combine credentials across the auth/runtime/domain or service pools.

Global `auth_account_attempts`, `auth_login_attempts`, `known_devices` follow
the `users`/`auth_sessions` identity model: no tenant key/RLS. Exact column ACLs
permit only `app_auth` reads/inserts/counter or revocation updates; every other
runtime/service role and PUBLIC has zero table/column access; the unreachable
credential writer has only the stated cleanup columns and counter DELETE. `app_auth` also reads
only tenant `id,status` for suspension checks. API keys remain tenant-bound.

**No credentials or application LOGIN role is created by migrations.** A
production operator must provision separate login identities out-of-band. The
local Compose migrator provisions environment-backed logins for these six API
capabilities and separate queue/outbox capabilities; see [README.md](README.md) for the rehearsal and password policy.
Use `psql` interactively with a privileged administrator and commands such as:

```sql
CREATE ROLE opensite_runtime_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
CREATE ROLE opensite_auth_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
CREATE ROLE opensite_domain_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT app_runtime TO opensite_runtime_login;
GRANT app_auth TO opensite_auth_login;
GRANT app_domain_resolver TO opensite_domain_login;
```

For initial installation only, provision an equally restricted, dedicated
offline LOGIN and grant it **only** `app_admin_bootstrap`. It must be LOGIN
INHERIT, NOSUPERUSER/NOBYPASSRLS/NOCREATEDB/NOCREATEROLE/NOREPLICATION, have
no app table/column rights, and belong to no other role. The CLI rejects
migrator credentials and mixed capabilities. Never grant the writer role;
revoke/remove the bootstrap LOGIN after successful initialization.

Build `cmd/bootstrap-admin`, set its DSN through `OSC_BOOTSTRAP_DATABASE_URL`
(or `DATABASE_URL`), and supply the password via redirected stdin or
`OSC_BOOTSTRAP_PASSWORD`. Use `--email` and `--tenant-name`; no password flag
exists. Do not type passwords into an echoing terminal or place them in shell
history. The PHC uses pinned Argon2id m=19 MiB, t=2, p=1 with a fresh salt.
No owner is overwritten: any owner in any tenant causes atomic refusal.
Run bootstrap at READ COMMITTED; stronger snapshot isolation is rejected.

Then use `psql`'s interactive `\password opensite_runtime_login` command (and
similarly for the other two users). Do not put passwords into SQL files, shell
history, GitHub Actions logs or version control. Provide DSNs through secret
management at deployment time. The migrator retains its own, separately
protected administrator identity.

The `app.tenant_id` GUC is not authentication. The Go API must authenticate the
caller, check role/scope and resource membership, and set transaction-local
RLS tenant context via a parameterized query. If an untrusted caller could
submit arbitrary SQL, they could set the GUC themselves.

`app_auth` can SELECT credential hashes, so its login is as sensitive as an
application signing secret. Avoid giving it to the Astro renderer.

`app_domain_resolver` returns only verified domain mappings and published site
identities. The Go service enforces Host normalization and internal service
authorization; the role does not make DNS validation or TLS authorization
unnecessary.

## Deployment constraint

V1 supports exactly one OpenSite application database per PostgreSQL cluster.
All capability roles (`app_runtime`, `app_auth`, `app_customer_auth`, ...) are
cluster-wide, so a second OpenSite database on the same cluster would share
their memberships and grants. The local deployment preflight refuses a cluster that
already hosts another OpenSite database, and operators must `REVOKE CONNECT ON
DATABASE ... FROM PUBLIC` so capability logins reach only their own database.

### Reliability integration (INT-25)

The River worker login may SET ROLE only to `app_queue_worker`; optional Outbox
composition uses a separate login allowed only `app_outbox_worker`. Neither is
permitted to SET ROLE to either DDL owner or `app_outbox_tenant_broker`.
The broker is NOLOGIN/NOBYPASSRLS and has no memberships; one controlled function enumerates active Outbox tenant IDs; a separate
aggregate-only metrics function exposes backlog/DLQ counts and age using
only tenant_id/status/created_at SELECT.
It has no payload or table-wide privilege. Queue ownership remains isolated in
`app_river_migrator`; the application and upstream migration histories are separate.
`OUTBOX_ENABLED` defaults off and a missing Sender fails before any claim.

Identity integration also grants the function owner only tenant-scoped buyer-role seed INSERT columns, so first-owner bootstrap composes with the customers tenant trigger. Bootstrap restores the caller tenant GUC. Counter functions sample database time and expose no clock parameter. The execute-only bounded retention function is available to app_auth; no authentication caller receives direct DELETE. A future hourly maintenance caller must drain small batches; scheduling is outside this migration.

### Staff login session activity

The login migration adds `auth_sessions.last_seen_at` and grants `app_auth`
only that additional UPDATE column, preserving the existing revocation update
and dedicated LOGIN boundary. Existing session activity is backfilled from
`created_at`; no runtime credential access or tenant/RLS authority is added.

### Customer authentication

Provision an independent `opensite_customer_auth_login` with the same restricted
LOGIN flags shown above, and grant only `app_customer_auth` with SET allowed and
ADMIN denied. Do not grant `app_customer_executor` to any login. Supply
`OPENSITE_CUSTOMER_AUTH_DATABASE_URL` or environment-backed CUSTOMER_AUTH database
components, a pinned HTTPS `OPENSITE_STOREFRONT_ORIGIN`, and a CSPRNG-generated
32–128 byte base64url `OPENSITE_CUSTOMER_CSRF_SECRET`. Secrets are never committed
or logged. SMTP remains disabled unless explicitly configured for loopback and
non-production use; customer challenges are restricted to test recipients.
The local migrator/restore validates the independent customer login credential
before migration or target-database writes. Fresh Compose HBA rules fence this
login to the same exact application database as the other seven component logins.
The recovery drill includes real local proof/login/cart witnesses and restores
the authentication tables with the all-table archive; see [README.md](README.md).

### Fulfillment and refund coordinator

`app_returns_service` is a separate NOLOGIN, NOINHERIT, NOBYPASSRLS capability.
The eighth local component identity is `opensite_returns_login`, granted only
`app_returns_service`, with LOGIN/INHERIT and NOSUPERUSER/NOBYPASSRLS/NOCREATEDB/
NOCREATEROLE/NOREPLICATION; the capability itself remains NOINHERIT. Supply
`OPENSITE_RETURNS_DATABASE_URL`, or `OPENSITE_RETURNS_DB_PASSWORD` together with
`OPENSITE_DB_HOST` and `OPENSITE_DB` (default `opensite`). The local migrator and
restore require an independent valid password before migration/target writes.
Fresh Compose HBA confines this identity to the application database over IPv4,
IPv6 and Unix sockets; wrong passwords and maintenance databases are rejected.
Bootstrap validates exact membership and effective privileges, owns the pool
lifecycle, and exposes authorized transactions as `Config.ReturnsCoordinator.Runner`
for a trusted Returns composition consumer. It does not populate `Returns.Service`.
An absent binding logs the setting names only and preserves current HTTP behavior.
An invalid configured binding refuses startup without echoing credentials.

Only a trusted refund coordinator may receive a dedicated login with that role;
never grant it to `app_runtime`, the Astro renderer or a public query identity.
Migration grants permit frozen order/return reads and the controlled refund
prepare/complete and ledger-post functions. They permit no direct return writes,
inventory adjustment, DDL or role membership administration.

To finish an already admitted C-04 refund atomically, it may SELECT only the
claim identity/hash/token/state columns and UPDATE only the completion state,
status, body reference and timestamp of `idempotency_records`. Its FORCE-RLS
policies additionally require the current tenant and operation `refundReturn`.
It cannot INSERT/claim requests, change claim tokens or complete another operation.

The coordinator must use authenticated tenant transactions through `pgxadapter`
and server-provided captured-payment/account bindings. Do not provision membership
in an executor/DDL-owner role. Credentials and provider configuration remain
out-of-band. A missing trusted binding leaves the refund HTTP operation unavailable.


### Tenant business authorization (C-02c)

The API composes staff authentication with the tenant module's private `app_auth`
repository. Session and bearer-key requests must pass current user, tenant,
membership and exact role/scope checks. Keys are bound to one tenant; only a
SHA-256 digest of their random 32-byte secret is stored. Key listing omits both
the secret and digest; creation returns the plaintext only once with no-store.
The fixed role map is in `APP_LAYER_DECISIONS.md` A3; unknown roles/scopes deny.

`app_auth` receives EXECUTE on create/revoke/touch key capabilities and the
boolean transaction-authorization capability, without INSERT/UPDATE/DELETE on
`api_keys`. `app_runtime` receives only the latter boolean capability, and
continues to have no key/session credential reads. Each admitted tenant write
checks that capability within its own transaction before domain work. The
NOLOGIN, memberless `app_credential_writer` owns these functions and has only
exactly tested columns plus tenant-scoped membership/key lock policies. These
locks keep a concurrent membership/session/key revocation from overtaking an
already-authorized write transaction. `tests/db_role_matrix.sql` asserts grants,
function owners/EXECUTE/search paths and the membership lock policy.

The database recheck uses database expiration and revocation state. Existing
session idle-time policy remains in the login service; the deferred H-4 card owns
DB-clock idle/absolute policy and flooding hardening. The default 30-minute idle
window exceeds the production request deadline; custom shorter idle policies
still need H-4 transaction-level policy alignment.

