#!/usr/bin/env bash
# Responsibility: run actual catalog/media HTTP tests with least-privilege logins.
# Dependencies: migrated disposable PG18, Go, psql and committed fixture SQL.
# Caller: foundation CI / developer gate. No production database is authorized.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
[[ "${OPENSITE_DISPOSABLE_PG:-}" == 1 ]] || { echo 'BLOCKED: OPENSITE_DISPOSABLE_PG=1 required for owned test database' >&2; exit 4; }
: "${DATABASE_URL:?disposable migrated PostgreSQL URL required}"
fixture_password="${C07_FIXTURE_PASSWORD:-$(python3 -c 'import secrets;print(secrets.token_urlsafe(32))')}"
C07_FIXTURE_PASSWORD="$fixture_password" psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 <<'SQL'
\getenv fixture_password C07_FIXTURE_PASSWORD
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION INHERIT',name)
FROM (VALUES('c07_runtime'),('c07_media')) AS fixture(name) WHERE NOT EXISTS(SELECT FROM pg_roles WHERE rolname=name) \gexec
SELECT format('ALTER ROLE %I PASSWORD %L',name,:'fixture_password')
FROM (VALUES('c07_runtime'),('c07_media')) AS fixture(name) \gexec
GRANT app_runtime TO c07_runtime WITH INHERIT TRUE;
GRANT app_media_service TO c07_media WITH INHERIT TRUE;
DO $check$ BEGIN
 IF EXISTS(SELECT FROM pg_roles WHERE rolname IN ('c07_runtime','c07_media')
  AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication OR NOT rolcanlogin))
 THEN RAISE EXCEPTION 'C07_FIXTURE_ROLE_NOT_LEAST_PRIVILEGE'; END IF;
END $check$;
SQL
psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 -f tests/fixtures/catalog_api.sql
runtime_url() {
 python3 - "$DATABASE_URL" "$1" <<'PY'
import sys
from urllib.parse import urlsplit,urlunsplit
u=urlsplit(sys.argv[1]);host=u.hostname or 'localhost'
if ':' in host:host='['+host+']'
if u.port:host+=':'+str(u.port)
print(urlunsplit((u.scheme,sys.argv[2]+'@'+host,u.path,u.query,u.fragment)))
PY
}
export C07_DATABASE_URL="${C07_DATABASE_URL:-$(runtime_url c07_runtime)}"
export C07_MEDIA_DATABASE_URL="${C07_MEDIA_DATABASE_URL:-$(runtime_url c07_media)}"
python3 - <<'PYROLE'
import os
from urllib.parse import urlsplit,parse_qs
for key,expected in [('C07_DATABASE_URL','c07_runtime'),('C07_MEDIA_DATABASE_URL','c07_media')]:
 u=urlsplit(os.environ[key])
 if u.username != expected or 'user' in parse_qs(u.query,keep_blank_values=True):
  raise SystemExit('FAIL: catalog/media HTTP gate requires dedicated fixture login')
PYROLE
PGPASSWORD="$fixture_password" go test -race -count=1 -v -tags pgx ./internal/modules/catalog ./internal/modules/media
psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 <<'SQL'
DO $check$ BEGIN
 IF NOT EXISTS(SELECT FROM app.product_media WHERE tenant_id='11111111-1111-4111-8111-111111111111'
  AND media_id='55555555-5555-4555-8555-555555555555' AND role='gallery')
 THEN RAISE EXCEPTION 'C07_PINNED_DELETE_LOST_ASSOCIATION'; END IF;
 IF NOT EXISTS(SELECT FROM app.sites WHERE tenant_id='11111111-1111-4111-8111-111111111111'
  AND published_release_id='99999999-9999-4999-8999-999999999999')
 THEN RAISE EXCEPTION 'C07_PINNED_RELEASE_NOT_PUBLISHED'; END IF;
 -- Staging blob was taken over only with the verified LocalFS content key.
 IF NOT EXISTS(SELECT FROM app.media_blobs WHERE id='66666666-6666-4666-8666-666666666667' AND status='ready'
  AND backend='local' AND storage_key=substr(encode(sha256,'hex'),1,2)||'/'||substr(encode(sha256,'hex'),3,2)||'/'||encode(sha256,'hex'))
 THEN RAISE EXCEPTION 'C07_STAGING_BLOB_NOT_VERIFIED_TAKEOVER'; END IF;
 IF NOT EXISTS(SELECT FROM app.media_blobs WHERE id='66666666-6666-4666-8666-666666666668' AND status='deleting' AND storage_key='fixture-deleting')
  OR EXISTS(SELECT FROM app.media_assets WHERE blob_id='66666666-6666-4666-8666-666666666668')
 THEN RAISE EXCEPTION 'C07_DELETING_BLOB_REVIVED'; END IF;
 IF EXISTS(SELECT FROM app.product_media WHERE product_id='33333333-3333-4333-8333-333333333336')
 THEN RAISE EXCEPTION 'C07_ARCHIVED_PRODUCT_LINKED'; END IF;
END $check$;
SQL
echo 'PASS: real catalog/media HTTP gates; pinned release, product association and blob takeover rules retained'
