#!/usr/bin/env bash
# Requires sqlc v1.31.1 and the Go version pinned in go.mod; pgx v5.10.0 comes from committed go.sum.
# Unlike the offline static checker, this performs REAL sqlc generation and
# Go typechecking against the actual pgx implementation.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
command -v sqlc >/dev/null || { echo "BLOCKED: sqlc v1.31.1 binary required" >&2; exit 4; }
command -v go >/dev/null || { echo "BLOCKED: Go required" >&2; exit 4; }
# Go toolchain must equal the go.mod directive (CI uses GOTOOLCHAIN=local with the same pin).
want_go="go$(sed -nE 's/^go ([0-9.]+)$/\1/p' go.mod)"
have_go="$(go env GOVERSION)"
if [[ "$have_go" != "$want_go" ]]; then
  if [[ "${OPENSITE_ALLOW_GO_MISMATCH:-}" == 1 ]]; then
    echo "WARN: $have_go != pinned $want_go (OPENSITE_ALLOW_GO_MISMATCH=1)" >&2
  else
    echo "BLOCKED: need exactly $want_go, found $have_go" >&2
    exit 4
  fi
fi
actual_sqlc="$(sqlc version)"
if [[ "$actual_sqlc" != "v1.31.1" ]]; then
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
