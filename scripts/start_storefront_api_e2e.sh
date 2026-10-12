#!/usr/bin/env bash
# Playwright owns this foreground API process and its timeout/termination.
# Migrates/seeds a disposable DB, then serves through a resolver-only login.
set -euo pipefail
set +x
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
[[ -n "${DATABASE_URL:-}" ]] || { echo 'E2E API requires DATABASE_URL for a disposable PostgreSQL database.' >&2; exit 2; }
export OSC_STOREFRONT_SAMPLE_SEED=1
./scripts/migrate.sh
./scripts/seed_storefront.sh
# Provision without putting a password or DSN in argv, source files, or output.
OSC_PUBLIC_DATABASE_URL="$(python3 - <<'PY'
import os, secrets, subprocess, sys
from urllib.parse import urlsplit, urlunsplit, quote, unquote, parse_qsl
source = urlsplit(os.environ['DATABASE_URL'])
if source.scheme not in ('postgres', 'postgresql') or not source.hostname or not source.path:
    sys.exit('E2E DATABASE_URL must be a PostgreSQL TCP URL with a database name.')
allowed = {'sslmode': 'PGSSLMODE', 'connect_timeout': 'PGCONNECT_TIMEOUT'}
params = parse_qsl(source.query, strict_parsing=True)
if source.fragment or any(key not in allowed for key, _ in params):
    sys.exit('E2E DATABASE_URL supports only sslmode and connect_timeout query fields.')
password = secrets.token_hex(32)
role = 'c09_public_e2e'
# A reused cluster role must already be OUR restricted disposable fixture.
# Never repair, assume control of, or grant capabilities to an unrelated role.
sql = """
BEGIN;
SELECT pg_advisory_xact_lock(18000902::bigint);
DO $provision$
DECLARE r pg_roles; role_oid oid;
BEGIN
 SELECT * INTO r FROM pg_roles WHERE rolname='c09_public_e2e';
 IF FOUND THEN
  IF NOT r.rolcanlogin OR NOT r.rolinherit OR r.rolsuper OR r.rolbypassrls
   OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication
   OR shobj_description(r.oid,'pg_authid') IS DISTINCT FROM 'C-09a disposable storefront E2E login'
   OR (SELECT count(*) FROM pg_auth_members WHERE member=r.oid)<>1
   OR NOT EXISTS(SELECT FROM pg_auth_members WHERE member=r.oid AND roleid='app_domain_resolver'::regrole AND set_option AND NOT admin_option)
   OR EXISTS(SELECT FROM pg_class c, LATERAL aclexplode(c.relacl) a WHERE a.grantee=r.oid)
   OR EXISTS(SELECT FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a WHERE a.grantee=r.oid)
   OR EXISTS(SELECT FROM pg_proc p, LATERAL aclexplode(p.proacl) a WHERE a.grantee=r.oid) THEN
   RAISE EXCEPTION 'E2E_LOGIN_NOT_OWN_RESTRICTED_FIXTURE';
  END IF;
 ELSE
  CREATE ROLE c09_public_e2e LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  COMMENT ON ROLE c09_public_e2e IS 'C-09a disposable storefront E2E login';
  GRANT app_domain_resolver TO c09_public_e2e;
 END IF;
END $provision$;
ALTER ROLE c09_public_e2e PASSWORD 'PASSWORD_PLACEHOLDER';
COMMIT;
""".replace('PASSWORD_PLACEHOLDER', password)
env = dict({key: value for key, value in os.environ.items() if not key.startswith('PG')}, PGHOST=source.hostname, PGPORT=str(source.port or 5432),
           PGDATABASE=unquote(source.path[1:]), PGUSER=unquote(source.username or ''),
           PGPASSWORD=unquote(source.password or ''), PGCONNECT_TIMEOUT='10')
for key, value in params:
    env[allowed[key]] = value
result = subprocess.run(['psql','-X','-q','-v','ON_ERROR_STOP=1'], input=sql, text=True,
                        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
if result.returncode:
    sys.exit('E2E restricted login provisioning failed; inspect role ownership/permissions without printing credentials.')
host = source.hostname
if ':' in host:
    host = '[' + host + ']'
if source.port:
    host += ':' + str(source.port)
print(urlunsplit((source.scheme, quote(role, safe='') + ':' + quote(password, safe='') + '@' + host,
                 source.path, source.query, '')))
PY
)"
export OSC_PUBLIC_DATABASE_URL
# Privileged fixture connection must not reach the API process environment.
unset DATABASE_URL OPENSITE_DATABASE_URL OPENSITE_MEDIA_DATABASE_URL C07_DATABASE_URL C07_MEDIA_DATABASE_URL C07_FIXTURE_PASSWORD PGDATABASE PGPASSWORD PGUSER PGHOST PGPORT PGSSLMODE PGCONNECT_TIMEOUT
mkdir -p .work/c09-e2e
go build -tags pgx -o .work/c09-e2e/api ./cmd/api
export OSC_API_ADDR=127.0.0.1:4175
exec .work/c09-e2e/api
