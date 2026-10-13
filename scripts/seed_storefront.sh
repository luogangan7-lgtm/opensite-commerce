#!/usr/bin/env bash
# Opt-in sample data for a disposable migrated PostgreSQL 18 database.
# Called by E2E startup or explicitly by developers; no production routing.
set -euo pipefail
set +x
[[ "${OSC_STOREFRONT_SAMPLE_SEED:-}" == 1 ]] || { echo 'Sample seed requires OSC_STOREFRONT_SAMPLE_SEED=1 (disposable database only).' >&2; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  python3 - <<'PYSEED'
import os, subprocess, sys
from urllib.parse import urlsplit, unquote, parse_qsl
url = urlsplit(os.environ['DATABASE_URL'])
allowed = {'sslmode': 'PGSSLMODE', 'connect_timeout': 'PGCONNECT_TIMEOUT'}
params = parse_qsl(url.query, strict_parsing=True)
if url.scheme not in ('postgres', 'postgresql') or not url.hostname or not url.path.strip('/') or url.fragment:
    sys.exit('Sample DATABASE_URL must be a PostgreSQL TCP URL with a database name.')
if any(key not in allowed for key, _ in params):
    sys.exit('Sample DATABASE_URL supports only sslmode and connect_timeout query fields.')
env = dict({key: value for key, value in os.environ.items() if not key.startswith('PG')}, PGHOST=url.hostname, PGPORT=str(url.port or 5432),
           PGDATABASE=unquote(url.path[1:]), PGUSER=unquote(url.username or ''),
           PGPASSWORD=unquote(url.password or ''), PGCONNECT_TIMEOUT='10')
for key, value in params:
    env[allowed[key]] = value
command = ['psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1']
version = subprocess.run(command + ['-At'], input='SHOW server_version_num;\n', text=True,
                         env=env, capture_output=True)
if version.returncode:
    sys.exit('Sample database connection failed.')
if not version.stdout.strip().isdigit() or not 180000 <= int(version.stdout.strip()) < 190000:
    sys.exit('PostgreSQL 18 required.')
with open('tests/fixtures/storefront_seed.sql') as source:
    sys.exit(subprocess.run(command, stdin=source, env=env).returncode)
PYSEED
else
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -U postgres -d opensite -v ON_ERROR_STOP=1)
  version_num="$(printf 'SHOW server_version_num;\n' | "${PSQL[@]}" -At)"
  [[ "$version_num" =~ ^18[0-9]{4}$ ]] || { echo 'PostgreSQL 18 required.' >&2; exit 5; }
  "${PSQL[@]}" < tests/fixtures/storefront_seed.sql
fi
