#!/usr/bin/env bash
# PostgreSQL 18 forward-only, checksum-locked migrations.
# Either set DATABASE_URL and install psql, or use Docker Compose.
# Each migration is committed together with its checksum under a DB advisory
# transaction lock, so concurrent operators cannot half-apply/skip a version.
# In-development migrations (migrations/pending/, see scripts/pending_migrations.py)
# are refused unless OPENSITE_ALLOW_PENDING_MIGRATIONS=1 (disposable dev/CI DBs
# only); they are then applied from a temp dir, numbered after the landed history.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
shopt -s nullglob
MIGRATIONS_DIR=migrations
pending=(migrations/pending/*)
if ((${#pending[@]} > 0)); then
  if [[ "${OPENSITE_ALLOW_PENDING_MIGRATIONS:-}" != 1 ]]; then
    echo "Refusing: migrations/pending/ is not empty; land them (scripts/land_migration.sh) or set OPENSITE_ALLOW_PENDING_MIGRATIONS=1 on a disposable dev/CI database" >&2
    exit 7
  fi
  [[ -n "${DATABASE_URL:-}" ]] || { echo "Pending migrations need DATABASE_URL (the Compose container cannot see the temp dir)" >&2; exit 7; }
  MIGRATIONS_DIR="$(mktemp -d)"
  trap 'rm -rf "$MIGRATIONS_DIR"' EXIT
  python3 scripts/pending_migrations.py materialize "$MIGRATIONS_DIR" || exit 2
fi
if [[ -n "${DATABASE_URL:-}" ]]; then
  command -v psql >/dev/null || { echo "psql not installed" >&2; exit 4; }
  PSQL=(psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1)
  INCLUDE_ROOT="$(cd "$MIGRATIONS_DIR" && pwd)"
else
  command -v docker >/dev/null || { echo "Neither DATABASE_URL nor Docker is available" >&2; exit 4; }
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -U postgres -d opensite -v ON_ERROR_STOP=1)
  INCLUDE_ROOT="/migrations"
fi
{ read -r version_num; read -r current_db; } <<<"$(printf "SHOW server_version_num;\nSELECT current_database();\n" | "${PSQL[@]}" -At)" || true
version_num="$(tr -d '[:space:]' <<<"${version_num:-}")"
case "${current_db:-}" in
  postgres|template0|template1) echo "Refusing to migrate system database ${current_db}" >&2; exit 6 ;;
esac
if ! [[ "$version_num" =~ ^[0-9]+$ ]] || (( version_num < 180000 || version_num >= 190000 )); then
  echo "PostgreSQL 18 required (found server_version_num=${version_num})" >&2
  exit 5
fi
files=("$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.sql)
((${#files[@]} > 0)) || { echo "No migrations" >&2; exit 3; }
# Prevent accidentally skipping, renaming or duplicating an ordered migration.
expected_number=1
for file in "${files[@]}"; do
  base="${file##*/}"
  number="${base%%_*}"
  version="${base%.sql}"
  [[ "$version" =~ ^[0-9]{3}_[a-z][a-z0-9_]*$ ]] || { echo "Invalid migration: $version" >&2; exit 2; }
  [[ "$number" =~ ^[0-9]{3}$ ]] || { echo "Bad migration prefix: $base" >&2; exit 2; }
  if (( 10#$number != expected_number )); then
    echo "Migration numbering must be contiguous: expected $expected_number, found $number ($base)" >&2
    exit 2
  fi
  (( expected_number += 1 ))
done
versions=()
for file in "${files[@]}"; do
  name="${file##*/}"
  versions+=("${name%.sql}")
done
known_versions="$(IFS=,; echo "${versions[*]}")"
for file in "${files[@]}"; do
  base="${file##*/}"
  version="${base%.sql}"
  checksum="$(sha256sum "$file" | awk '{print $1}')"
  echo "CHECK $version"
  # File names are validated above. The file path is fixed by this repository.
  # SQL values use :'var'; the include path is one psql variable argument.
  "${PSQL[@]}" -v m_version="$version" -v m_checksum="$checksum" -v m_known_versions="$known_versions" -v m_include="$INCLUDE_ROOT/$base" <<SQL
BEGIN;
SELECT pg_advisory_xact_lock(18004517::bigint);
CREATE TABLE IF NOT EXISTS public.schema_migrations (
  version text PRIMARY KEY,
  checksum text NOT NULL,
  installed_at timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON public.schema_migrations FROM PUBLIC;
-- Fail BEFORE applying a new file if the database contains orphaned history.
SELECT EXISTS (
  SELECT 1 FROM public.schema_migrations
  WHERE NOT (version = ANY(string_to_array(:'m_known_versions', ',')))
) AS m_unknown_history \gset
\if :m_unknown_history
  SELECT 'MIGRATION_MISSING_SOURCE_FILE'::integer;
\endif
SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version = :'m_version') AS m_installed \gset
\if :m_installed
  SELECT (checksum = :'m_checksum') AS m_matched
    FROM public.schema_migrations WHERE version = :'m_version' \gset
  \if :m_matched
    \echo Already applied: $version (checksum matched)
  \else
    SELECT 'MIGRATION_CHECKSUM_DRIFT'::integer;
  \endif
\else
  SELECT EXISTS(
    SELECT 1 FROM public.schema_migrations WHERE version > :'m_version'
  ) AS m_newer_installed \gset
  \if :m_newer_installed
    SELECT 'MIGRATION_OUT_OF_ORDER'::integer;
  \endif
  \i :m_include
  INSERT INTO public.schema_migrations(version, checksum)
    VALUES (:'m_version', :'m_checksum');
  \echo Applied: $version
\endif
COMMIT;
SQL
done
# Detect applied DB migrations removed from source control (or unknown history).
# All migration names are constrained to [0-9]{3}_[a-z][a-z0-9_]* before use.
"${PSQL[@]}" -v m_known_versions="$known_versions" <<'SQL'
BEGIN;
SELECT pg_advisory_xact_lock(18004517::bigint);
SELECT EXISTS (
  SELECT 1 FROM public.schema_migrations
  WHERE NOT (version = ANY(string_to_array(:'m_known_versions', ',')))
) AS m_unknown_history \gset
\if :m_unknown_history
  SELECT 'MIGRATION_MISSING_SOURCE_FILE'::integer;
\endif
COMMIT;
SQL
printf '%s\n' 'All migrations applied, in order, with checksums and history verified.' 
