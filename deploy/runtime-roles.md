# Provision separate PostgreSQL login identities (required before production API)

The migrations create restricted **NOLOGIN** group roles, including:

| Group role | Purpose | Safety boundary |
|---|---|---|
| `app_runtime` | Tenant-scoped domain queries | FORCE RLS, no credential hashes, no DDL/DELETE/TRUNCATE |
| `app_auth` | Credential/session lookup | Not exposed to public/Agent query APIs |
| `app_domain_resolver` | Verified hostname lookup | No credential or media access |
| `app_media_service` | Controlled media lifecycle queries | Separate trusted service pool; no general runtime credential access |

**No credentials or application LOGIN role is created by migrations.** A
production operator must provision separate login identities out-of-band. The
local Compose migrator provisions environment-backed logins for these four
capabilities; see [README.md](README.md) for the rehearsal and password policy.
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
their memberships and grants. The local deployment preflight refuses a cluster that
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
