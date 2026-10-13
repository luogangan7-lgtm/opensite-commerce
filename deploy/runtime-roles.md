# Provision separate PostgreSQL login identities (required before production API)

The migrations create three restricted **NOLOGIN** group roles:

| Group role | Purpose | Safety boundary |
|---|---|---|
| `app_runtime` | Tenant-scoped domain queries | FORCE RLS, no credential hashes, no DDL/DELETE/TRUNCATE |
| `app_auth` | Credential/session lookup | Not exposed to public/Agent query APIs |
| `app_domain_resolver` | Verified hostname lookup | No credential or media access |

**No credentials or application LOGIN role is created by migrations.** A
production operator must provision separate login identities out-of-band.
Use `psql` interactively with a privileged administrator and commands such as:

```sql
CREATE ROLE opensite_runtime_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
CREATE ROLE opensite_auth_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
CREATE ROLE opensite_domain_login LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT app_runtime TO opensite_runtime_login;
GRANT app_auth TO opensite_auth_login;
GRANT app_domain_resolver TO opensite_domain_login;
```

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
their memberships and grants. Deployment preflight will refuse a cluster that
already hosts another OpenSite database, and operators must `REVOKE CONNECT ON
DATABASE ... FROM PUBLIC` so capability logins reach only their own database.

### Reliability integration (INT-25)

The River worker login may SET ROLE only to `app_queue_worker`; optional Outbox
composition uses a separate login allowed only `app_outbox_worker`. Neither is
permitted to SET ROLE to either DDL owner or `app_outbox_tenant_broker`.
The broker is NOLOGIN/NOBYPASSRLS and has no memberships; its sole controlled
function enumerates active Outbox tenant IDs using only tenant_id/status SELECT.
It has no payload or table-wide privilege. Queue ownership remains isolated in
`app_river_migrator`; the application and upstream migration histories are separate.
`OUTBOX_ENABLED` defaults off and a missing Sender fails before any claim.

### Fulfillment and refund coordinator

`app_returns_service` is a separate NOLOGIN, NOINHERIT, NOBYPASSRLS capability.
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
