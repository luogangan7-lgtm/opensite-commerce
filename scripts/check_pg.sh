#!/usr/bin/env bash
# Runs against an expendable PostgreSQL 18 database; requires psql.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  command -v psql >/dev/null
  PSQL=(psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1)
else
  command -v docker >/dev/null || { echo "DATABASE_URL or Docker required" >&2; exit 4; }
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -U postgres -d opensite -v ON_ERROR_STOP=1)
fi
./scripts/migrate.sh
./scripts/migrate.sh  # checksum/second-run idempotency
for test_file in tests/db_security_audit.sql tests/db_smoke.sql tests/db_isolation.sql tests/db_role_boundaries.sql tests/db_release_integrity.sql tests/db_security_hardening.sql tests/db_role_matrix.sql tests/db_connection_reuse.sql tests/db_security_services.sql tests/db_publish_isolation.sql tests/db_sealed_media.sql tests/db_reliability.sql; do
  echo "RUN $test_file"
  "${PSQL[@]}" < "$test_file"
done
