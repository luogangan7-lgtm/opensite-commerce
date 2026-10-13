#!/usr/bin/env bash
# Controlled installation of pinned River in queue; app migration history stays
# in migrate.sh. Called by check_pg/upgrade and the explicit installation flow.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -n "${DATABASE_URL:-}" ]]; then
  GOTOOLCHAIN=local go run ./cmd/river-migrate "$@"
else
  # Compose has no published PG port. Build the pinned installer for that
  # expendable container; credentials stay in its environment, never arguments.
  command -v docker >/dev/null
  database="${RIVER_DATABASE_NAME:-opensite}"
  [[ "$database" =~ ^[a-zA-Z0-9_]+$ ]] || { echo 'Invalid River database name' >&2; exit 4; }
  compose=(docker compose -f deploy/compose.yaml)
  arch="$("${compose[@]}" exec -T postgres uname -m)"
  case "$arch" in aarch64|arm64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo 'Unsupported installer architecture' >&2; exit 4 ;; esac
  temp="$(mktemp -d "${TMPDIR:-/tmp}/opensite-river.XXXXXX")"
  container_bin="/tmp/opensite-river-$(basename "$temp")"
  cleanup() { "${compose[@]}" exec -T postgres rm -f "$container_bin" >/dev/null 2>&1 || true; rm -rf "$temp"; }
  trap cleanup EXIT
  GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$temp/installer" ./cmd/river-migrate
  "${compose[@]}" cp "$temp/installer" "postgres:$container_bin"
  "${compose[@]}" exec -T -e "DATABASE_URL=postgresql://postgres@localhost/$database?sslmode=disable" postgres sh -c 'export PGPASSWORD="$POSTGRES_PASSWORD"; exec "$@"' sh "$container_bin" "$@"
fi
