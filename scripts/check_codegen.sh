#!/usr/bin/env bash
# Run after installing sqlc v1.31.1, Go >=1.25 and fetching pgx v5.10.0.
# Unlike the offline static checker, this performs REAL sqlc generation and
# Go typechecking against the actual pgx implementation.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
command -v sqlc >/dev/null || { echo "BLOCKED: sqlc v1.31.1 binary required" >&2; exit 4; }
command -v go >/dev/null || { echo "BLOCKED: Go >=1.25 required" >&2; exit 4; }
# pgx v5.10 requires Go >=1.25; verify before generating.
go_ver="$(go version | sed -nE 's/.*go([0-9]+)\.([0-9]+)(\.[0-9]+)?.*/\1 \2/p')"
read -r major minor <<< "$go_ver"
if [[ -z "${major:-}" || -z "${minor:-}" ]] || ((major < 1 || (major == 1 && minor < 25))); then
  echo "BLOCKED: $(go version); pgx v5.10.0 requires Go >=1.25" >&2
  exit 4
fi
actual_sqlc="$(sqlc version)"
if [[ "$actual_sqlc" != *"1.31.1"* ]]; then
  echo "BLOCKED: expected sqlc v1.31.1, found $actual_sqlc" >&2
  exit 4
fi
if ! go list -m github.com/jackc/pgx/v5 | grep -Eq 'github.com/jackc/pgx/v5 v5\.10\.0$'; then
  echo "BLOCKED: pgx v5.10.0 must be pinned in go.mod/go.sum" >&2
  exit 4
fi
sqlc generate
test -f internal/db/db.go || { echo "FAIL: sqlc did not generate internal/db/db.go" >&2; exit 1; }
go test -tags pgx ./...
go vet -tags pgx ./...
echo 'PASS: real sqlc generation + pgx build/typecheck'
