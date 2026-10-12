#!/usr/bin/env bash
# PostgreSQL 18 forward-only, checksum-locked migrations.
# Either set DATABASE_URL and install psql, or use Docker Compose.
# Each migration is committed together with its checksum under a DB advisory
# transaction lock, so concurrent operators cannot half-apply/skip a version.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [[ -n "${DATABASE_URL:-}" ]]; then
  command -v psql >/dev/null || { echo "psql not installed" >&2; exit 4; }
  PSQL=(psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1)
  INCLUDE_ROOT="$ROOT/migrations"
else
  command -v docker >/dev/null || { echo "Neither DATABASE_URL nor Docker is available" >&2; exit 4; }
  PSQL=(docker compose -f deploy/compose.yaml exec -T postgres psql -X -q -U postgres -d opensite -v ON_ERROR_STOP=1)
  INCLUDE_ROOT="/migrations"
fi
version_num="$(printf "SHOW server_version_num;\n" | "${PSQL[@]}" -At | tr -d '[:space:]')"
if ! [[ "$version_num" =~ ^[0-9]+$ ]] || (( version_num < 180000 || version_num >= 190000 )); then
  echo "PostgreSQL 18 required (found server_version_num=${version_num})" >&2
  exit 5
fi
shopt -s nullglob
files=(migrations/[0-9][0-9][0-9]_*.sql)
((${#files[@]} > 0)) || { echo "No migrations" >&2; exit 3; }
# Prevent accidentally skipping, renaming or duplicating an ordered migration.
expected_number=1
for file in "${files[@]}"; do
  base="${file##*/}"
  number="${base%%_*}"
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
  [[ "$version" =~ ^[0-9]{3}_[a-z_]+$ ]] || { echo "Invalid migration: $version" >&2; exit 2; }
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
# All migration names are constrained to [0-9]{3}_[a-z_]+ before use.
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
