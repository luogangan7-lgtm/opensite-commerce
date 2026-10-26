#!/usr/bin/env bash
# Responsibility: construct a PostgreSQL URI for one explicit database name.
# Dependencies: bash only.
# Called by: scripts/check_pg_upgrade.sh, scripts/check_pg_races.sh and
#   tests/test_batch_hygiene.py (offline negative control).
# Invariants: the caller URL is never mutated; the path database is replaced;
#   every decoded (percent-encoded, case-insensitive) `dbname` query parameter
#   is removed so it cannot override the path; other parameters are preserved.
#   libpq parses the path first and then lets URI parameters replace it, which is
#   why leaving `?dbname=` would silently target the original database.
pg_url_for_database() {
  if [ "$#" -ne 2 ]; then
    echo "usage: pg_url_for_database <postgres-uri> <database>" >&2
    return 64
  fi
  local url=$1 database=$2 base query='' param key decoded kept=''
  if [[ $url == *\?* ]]; then
    base=${url%%\?*}
    query=${url#*\?}
  else
    base=$url
  fi
  base=${base%/}
  local out="${base%/*}/$database"
  if [[ -n $query ]]; then
    local IFS='&'
    for param in $query; do
      [ -n "$param" ] || continue
      key=${param%%=*}
      # One decode matches libpq: %64bname -> dbname, db%6Eame -> dbname.
      decoded=$(printf '%b' "${key//%/\\x}")
      if [[ ${decoded,,} == dbname ]]; then
        continue
      fi
      if [ -z "$kept" ]; then
        kept=$param
      else
        kept+="&$param"
      fi
    done
  fi
  [ -z "$kept" ] || out+="?$kept"
  printf '%s\n' "$out"
}
