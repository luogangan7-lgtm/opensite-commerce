# Provision separate PostgreSQL login identities (required before production API)

Migrations through `*_identity_auth.sql` create these restricted **NOLOGIN** roles.
All are NOSUPERUSER/NOBYPASSRLS/NOCREATEDB/NOCREATEROLE/NOREPLICATION/NOINHERIT,
with no outward memberships. Provision LOGIN credentials outside migrations.

| Group role | Purpose | Safety boundary |
|---|---|---|
| `app_runtime` | Tenant-scoped domain queries | FORCE RLS, no credential hashes, no DDL/DELETE/TRUNCATE |
| `app_auth` | Credential/session lookup | Not exposed to public/Agent query APIs |
| `app_domain_resolver` | Verified hostname lookup | No credential or media access |
| `app_domain_verifier` | Verify/retire domain mappings | Dedicated private verifier LOGIN; tenant RLS |
| `app_media_service` | Upload/blob readiness and media visibility | Dedicated private media LOGIN; tenant RLS |
| `app_publisher` | Execute seal/publish commands | Private publisher LOGIN; no direct pointer DML |
| `app_publish_executor` | Own seal/publish functions | No LOGIN or members; never provision credentials |
| `app_outbox_worker` | Claim/complete/fail delivery commands | Private worker LOGIN; fenced function capability |
| `app_reliability_executor` | Own outbox/idempotency functions | No LOGIN or members; never provision credentials |
| `app_inventory_manager` | Trusted inventory adjustments and reads | Private inventory manager LOGIN; tenant-scoped commands |
| `app_inventory_executor` | Own inventory mutations | No LOGIN or members; never provision credentials |
| `app_credential_writer` | Own bootstrap; minimum user hash/key issuance columns | No LOGIN or members; never provision credentials or grant this role |
| `app_admin_bootstrap` | Execute first-owner bootstrap only | Dedicated offline bootstrap LOGIN only; no table/column privileges |

The migrator's privileged LOGIN is a separate operator identity, not an
application role. It owns schema installation/checksums and is never used by
the API or bootstrap CLI. Business roles receive their own private LOGINs; do
not combine credentials across the auth/runtime/domain or service pools.

Global `auth_account_attempts`, `auth_login_attempts`, `known_devices` follow
the `users`/`auth_sessions` identity model: no tenant key/RLS. Exact column ACLs
permit only `app_auth` reads/inserts/counter or revocation updates; every other
application role and PUBLIC has zero table/column access. `app_auth` also reads
only tenant `id,status` for suspension checks. API keys remain tenant-bound.

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
