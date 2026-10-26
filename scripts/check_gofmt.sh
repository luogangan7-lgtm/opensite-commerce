#!/usr/bin/env bash
# Responsibility: fail if any tracked Go file differs from gofmt's output.
# Dependencies: git and the Go toolchain's gofmt (Go 1.27.2 in CI).
# Called by: scripts/check_all.sh (offline lane) and tests/test_batch_hygiene.py.
# Invariants: read-only scan; an optional root keeps the negative control
# isolated from the repository (the temp tree must be a git index).
set -euo pipefail
root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"
mapfile -d '' files < <(git ls-files -z -- '*.go')
if [ "${#files[@]}" -eq 0 ]; then
  echo "PASS: gofmt clean (no tracked Go files)"
  exit 0
fi
unformatted="$(gofmt -l "${files[@]}")"
if [ -n "$unformatted" ]; then
  echo "FAIL: gofmt required for:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi
echo "PASS: gofmt clean"
